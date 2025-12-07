package trader

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"nofx/decision"
	"nofx/logger"
	"nofx/market"
	"nofx/mcp"
	"nofx/pool"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// 当 AI 提供的 TP/SL 缺失或实际 R:R < 3 时的兜底比例
	fallbackTakeProfitPct = 0.008 // 0.8%
	// 兜底止损比例，结合 ATR 下限，避免“噪音级”止损
	fallbackStopLossPct = 0.005 // 0.5%
	// 监控与开关：通过环境变量快速启用/禁用
	envRelaxEnabled        = "NOFX_RELAX_ENABLED"
	envMinNotionalGuard    = "NOFX_MIN_NOTIONAL_GUARD_ENABLED"
	exchangeMinNotionalU99 = 100.0 // 交易所最小名义值硬阈值，提供安全边际
)

// AutoTraderConfig 自动交易配置（简化版 - AI全权决策）
type AutoTraderConfig struct {
	// Trader标识
	ID      string // Trader唯一标识（用于日志目录等）
	Name    string // Trader显示名称
	AIModel string // AI模型: "qwen" 或 "deepseek"

	// 交易平台选择
	Exchange string // "binance", "hyperliquid" 或 "aster"

	// 币安API配置
	BinanceAPIKey    string
	BinanceSecretKey string

	// Hyperliquid配置
	HyperliquidPrivateKey string
	HyperliquidWalletAddr string
	HyperliquidTestnet    bool

	// Aster配置
	AsterUser       string // Aster主钱包地址
	AsterSigner     string // Aster API钱包地址
	AsterPrivateKey string // Aster API钱包私钥

	CoinPoolAPIURL string

	// AI配置
	UseQwen     bool
	DeepSeekKey string
	QwenKey     string

	// 自定义AI API配置
	CustomAPIURL    string
	CustomAPIKey    string
	CustomModelName string

	// 扫描配置
	ScanInterval time.Duration // 扫描间隔（建议3分钟）

	// 账户配置
	InitialBalance float64 // 初始金额（用于计算盈亏，需手动设置）

	// 杠杆配置
	BTCETHLeverage  int // BTC和ETH的杠杆倍数
	AltcoinLeverage int // 山寨币的杠杆倍数

	// 风险控制（仅作为提示，AI可自主决定）
	MaxDailyLoss    float64       // 最大日亏损百分比（提示）
	MaxDrawdown     float64       // 最大回撤百分比（提示）
	StopTradingTime time.Duration // 触发风控后暂停时长

	// 仓位模式
	IsCrossMargin bool // true=全仓模式, false=逐仓模式

	// 币种配置
	DefaultCoins []string // 默认币种列表（从数据库获取）
	TradingCoins []string // 实际交易币种列表

	// 系统提示词模板
	SystemPromptTemplate string // 系统提示词模板名称（如 "default", "aggressive"）

	// 止损收紧节流（执行层保护）
	MinStopAdjustHold              time.Duration // 最小持仓时间，低于该值拒绝收紧止损
	MinStopAdjustROI               float64       // 最小ROI阈值（%），低于该值拒绝收紧止损
	MinStopAdjustATRs              float64       // 止损离入场的最小 ATR 倍数
	MinStopUpdateInterval          time.Duration // 连续两次收紧之间的最小间隔
	AllowImmediateTighteningROI    *bool         // 当 ROI 超过指定倍数时，是否允许忽略持仓时长
	ImmediateTighteningROIMultiple float64       // ROI 触发倍数（例如 2×MinStopAdjustROI）
}

// AutoTrader 自动交易器
type AutoTrader struct {
	id                    string // Trader唯一标识
	name                  string // Trader显示名称
	aiModel               string // AI模型名称
	exchange              string // 交易平台名称
	config                AutoTraderConfig
	trader                Trader // 使用Trader接口（支持多平台）
	mcpClient             *mcp.Client
	decisionLogger        *logger.DecisionLogger // 决策日志记录器
	initialBalance        float64
	dailyPnL              float64
	customPrompt          string   // 自定义交易策略prompt
	overrideBasePrompt    bool     // 是否覆盖基础prompt
	systemPromptTemplate  string   // 系统提示词模板名称
	defaultCoins          []string // 默认币种列表（从数据库获取）
	tradingCoins          []string // 实际交易币种列表
	lastResetTime         time.Time
	stopUntil             time.Time
	isRunning             bool
	startTime             time.Time                // 系统启动时间
	callCount             int                      // AI调用次数
	positionFirstSeenTime map[string]int64         // 持仓首次出现时间 (symbol_side -> timestamp毫秒)
	stopMonitorCh         chan struct{}            // 用于停止监控goroutine
	monitorWg             sync.WaitGroup           // 用于等待监控goroutine结束
	peakPnLCache          map[string]float64       // 最高收益缓存 (symbol -> 峰值盈亏百分比)
	peakPnLCacheMutex     sync.RWMutex             // 缓存读写锁
	lastBalanceSyncTime   time.Time                // 上次余额同步时间
	database              interface{}              // 数据库引用（用于自动更新余额）
	userID                string                   // 用户ID
	positionMeta          map[string]*positionMeta // 缓存 symbol_side 的入场详情，用于精确盈亏
	stopLossCache         map[string]float64       // 缓存止损价，供“只收紧不放宽”校验
	takeProfitCache       map[string]float64       // 缓存止盈价，便于动态调整
	positionMetaMutex     sync.Mutex               // 保护 positionMeta/stopLossCache 的并发访问
	consecutiveLosses     int                      // 连续亏损计数，驱动自动暂停
	lastStopUpdate        map[string]time.Time     // 最近一次收紧止损时间
	stopUpdateMutex       sync.RWMutex             // 保护 lastStopUpdate
}

const (
	minConfidence = 80   // 执行层硬性要求的最低置信度
	floatEpsilon  = 1e-6 // 浮点比较容差
)

func envBool(env string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(env)))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

type positionMeta struct {
	Side       string  // 持仓方向（long / short）
	EntryPrice float64 // 入场参考价
	Quantity   float64 // 成交数量
}

// NewAutoTrader 创建自动交易器
func NewAutoTrader(config AutoTraderConfig, database interface{}, userID string) (*AutoTrader, error) {
	// 止损节流参数默认值
	if config.MinStopAdjustHold == 0 {
		config.MinStopAdjustHold = 12 * time.Minute
	}
	if config.MinStopAdjustROI <= 0 {
		config.MinStopAdjustROI = 1.2
	}
	if config.MinStopAdjustATRs <= 0 {
		config.MinStopAdjustATRs = 0.6
	}
	if config.MinStopUpdateInterval == 0 {
		config.MinStopUpdateInterval = 3 * time.Minute
	}
	if config.ImmediateTighteningROIMultiple <= 0 {
		config.ImmediateTighteningROIMultiple = 2.0
	}
	if config.AllowImmediateTighteningROI == nil {
		defaultAllow := true
		config.AllowImmediateTighteningROI = &defaultAllow
	}

	// 设置默认值
	if config.ID == "" {
		config.ID = "default_trader"
	}
	if config.Name == "" {
		config.Name = "Default Trader"
	}
	if config.AIModel == "" {
		if config.UseQwen {
			config.AIModel = "qwen"
		} else {
			config.AIModel = "deepseek"
		}
	}

	mcpClient := mcp.New()

	// 初始化AI
	if config.AIModel == "custom" {
		// 使用自定义API
		mcpClient.SetCustomAPI(config.CustomAPIURL, config.CustomAPIKey, config.CustomModelName)
		log.Printf("🤖 [%s] 使用自定义AI API: %s (模型: %s)", config.Name, config.CustomAPIURL, config.CustomModelName)
	} else if config.UseQwen || config.AIModel == "qwen" {
		// 使用Qwen (支持自定义URL和Model)
		mcpClient.SetQwenAPIKey(config.QwenKey, config.CustomAPIURL, config.CustomModelName)
		if config.CustomAPIURL != "" || config.CustomModelName != "" {
			log.Printf("🤖 [%s] 使用阿里云Qwen AI (自定义URL: %s, 模型: %s)", config.Name, config.CustomAPIURL, config.CustomModelName)
		} else {
			log.Printf("🤖 [%s] 使用阿里云Qwen AI", config.Name)
		}
	} else {
		// 默认使用DeepSeek (支持自定义URL和Model)
		mcpClient.SetDeepSeekAPIKey(config.DeepSeekKey, config.CustomAPIURL, config.CustomModelName)
		if config.CustomAPIURL != "" || config.CustomModelName != "" {
			log.Printf("🤖 [%s] 使用DeepSeek AI (自定义URL: %s, 模型: %s)", config.Name, config.CustomAPIURL, config.CustomModelName)
		} else {
			log.Printf("🤖 [%s] 使用DeepSeek AI", config.Name)
		}
	}

	// 初始化币种池API
	if config.CoinPoolAPIURL != "" {
		pool.SetCoinPoolAPI(config.CoinPoolAPIURL)
	}

	// 设置默认交易平台
	if config.Exchange == "" {
		config.Exchange = "binance"
	}

	// 根据配置创建对应的交易器
	var trader Trader
	var err error

	// 记录仓位模式（通用）
	marginModeStr := "全仓"
	if !config.IsCrossMargin {
		marginModeStr = "逐仓"
	}
	log.Printf("📊 [%s] 仓位模式: %s", config.Name, marginModeStr)

	switch config.Exchange {
	case "binance":
		log.Printf("🏦 [%s] 使用币安合约交易", config.Name)
		trader = NewFuturesTrader(config.BinanceAPIKey, config.BinanceSecretKey)
	case "hyperliquid":
		log.Printf("🏦 [%s] 使用Hyperliquid交易", config.Name)
		trader, err = NewHyperliquidTrader(config.HyperliquidPrivateKey, config.HyperliquidWalletAddr, config.HyperliquidTestnet)
		if err != nil {
			return nil, fmt.Errorf("初始化Hyperliquid交易器失败: %w", err)
		}
	case "aster":
		log.Printf("🏦 [%s] 使用Aster交易", config.Name)
		trader, err = NewAsterTrader(config.AsterUser, config.AsterSigner, config.AsterPrivateKey)
		if err != nil {
			return nil, fmt.Errorf("初始化Aster交易器失败: %w", err)
		}
	default:
		return nil, fmt.Errorf("不支持的交易平台: %s", config.Exchange)
	}

	// 验证初始金额配置
	if config.InitialBalance <= 0 {
		return nil, fmt.Errorf("初始金额必须大于0，请在配置中设置InitialBalance")
	}

	// 初始化决策日志记录器（使用trader ID创建独立目录）
	logDir := fmt.Sprintf("decision_logs/%s", config.ID)
	decisionLogger := logger.NewDecisionLogger(logDir)

	// 设置默认系统提示词模板
	systemPromptTemplate := config.SystemPromptTemplate
	if systemPromptTemplate == "" {
		// feature/partial-close-dynamic-tpsl 分支默认使用 adaptive（支持动态止盈止损）
		systemPromptTemplate = "adaptive"
	}

	return &AutoTrader{
		id:                    config.ID,
		name:                  config.Name,
		aiModel:               config.AIModel,
		exchange:              config.Exchange,
		config:                config,
		trader:                trader,
		mcpClient:             mcpClient,
		decisionLogger:        decisionLogger,
		initialBalance:        config.InitialBalance,
		systemPromptTemplate:  systemPromptTemplate,
		defaultCoins:          config.DefaultCoins,
		tradingCoins:          config.TradingCoins,
		lastResetTime:         time.Now(),
		startTime:             time.Now(),
		callCount:             0,
		isRunning:             false,
		positionFirstSeenTime: make(map[string]int64),
		stopMonitorCh:         make(chan struct{}),
		monitorWg:             sync.WaitGroup{},
		peakPnLCache:          make(map[string]float64),
		peakPnLCacheMutex:     sync.RWMutex{},
		positionMeta:          make(map[string]*positionMeta),
		stopLossCache:         make(map[string]float64),
		takeProfitCache:       make(map[string]float64),
		positionMetaMutex:     sync.Mutex{},
		lastStopUpdate:        make(map[string]time.Time),
		lastBalanceSyncTime:   time.Now(), // 初始化为当前时间
		database:              database,
		userID:                userID,
	}, nil
}

// Run 运行自动交易主循环
func (at *AutoTrader) Run() error {
	at.isRunning = true
	log.Println("🚀 AI驱动自动交易系统启动")
	log.Printf("💰 初始余额: %.2f USDT", at.initialBalance)
	log.Printf("⚙️  扫描间隔: %v", at.config.ScanInterval)
	log.Println("🤖 AI将全权决定杠杆、仓位大小、止损止盈等参数")

	// 启动回撤监控
	at.startDrawdownMonitor()

	ticker := time.NewTicker(at.config.ScanInterval)
	defer ticker.Stop()

	// 首次立即执行
	if err := at.runCycle(); err != nil {
		log.Printf("❌ 执行失败: %v", err)
	}

	for at.isRunning {
		select {
		case <-ticker.C:
			if err := at.runCycle(); err != nil {
				log.Printf("❌ 执行失败: %v", err)
			}
		}
	}

	return nil
}

// Stop 停止自动交易
func (at *AutoTrader) Stop() {
	at.isRunning = false
	close(at.stopMonitorCh) // 通知监控goroutine停止
	at.monitorWg.Wait()     // 等待监控goroutine结束
	log.Println("⏹ 自动交易系统停止")
}

// autoSyncBalanceIfNeeded 自动同步余额（每10分钟检查一次，变化>5%才更新）
func (at *AutoTrader) autoSyncBalanceIfNeeded() {
	// 距离上次同步不足10分钟，跳过
	if time.Since(at.lastBalanceSyncTime) < 10*time.Minute {
		return
	}

	log.Printf("🔄 [%s] 开始自动检查余额变化...", at.name)

	// 查询实际余额
	balanceInfo, err := at.trader.GetBalance()
	if err != nil {
		log.Printf("⚠️ [%s] 查询余额失败: %v", at.name, err)
		at.lastBalanceSyncTime = time.Now() // 即使失败也更新时间，避免频繁重试
		return
	}

	// 优先提取总钱包余额（不会受到保证金占用影响）
	var actualBalance float64
	if walletBalance, ok := balanceInfo["totalWalletBalance"].(float64); ok && walletBalance > 0 {
		actualBalance = walletBalance
	} else if walletBalance, ok := balanceInfo["walletBalance"].(float64); ok && walletBalance > 0 {
		actualBalance = walletBalance
	} else if totalBalance, ok := balanceInfo["balance"].(float64); ok && totalBalance > 0 {
		actualBalance = totalBalance
	} else if availableBalance, ok := balanceInfo["available_balance"].(float64); ok && availableBalance > 0 {
		actualBalance = availableBalance
	} else if availableBalance, ok := balanceInfo["availableBalance"].(float64); ok && availableBalance > 0 {
		actualBalance = availableBalance
	} else {
		log.Printf("⚠️ [%s] 无法提取有效余额", at.name)
		at.lastBalanceSyncTime = time.Now()
		return
	}

	oldBalance := at.initialBalance

	// 防止除以零：如果初始余额无效，直接更新为实际余额
	if oldBalance <= 0 {
		log.Printf("⚠️ [%s] 初始余额无效 (%.2f)，直接更新为实际余额 %.2f USDT", at.name, oldBalance, actualBalance)
		at.initialBalance = actualBalance
		if at.database != nil {
			type DatabaseUpdater interface {
				UpdateTraderInitialBalance(userID, id string, newBalance float64) error
			}
			if db, ok := at.database.(DatabaseUpdater); ok {
				if err := db.UpdateTraderInitialBalance(at.userID, at.id, actualBalance); err != nil {
					log.Printf("❌ [%s] 更新数据库失败: %v", at.name, err)
				} else {
					log.Printf("✅ [%s] 已自动同步余额到数据库", at.name)
				}
			} else {
				log.Printf("⚠️ [%s] 数据库类型不支持UpdateTraderInitialBalance接口", at.name)
			}
		} else {
			log.Printf("⚠️ [%s] 数据库引用为空，余额仅在内存中更新", at.name)
		}
		at.lastBalanceSyncTime = time.Now()
		return
	}

	changePercent := ((actualBalance - oldBalance) / oldBalance) * 100

	// 变化超过5%才更新
	if math.Abs(changePercent) > 5.0 {
		log.Printf("🔔 [%s] 检测到余额大幅变化: %.2f → %.2f USDT (%.2f%%)",
			at.name, oldBalance, actualBalance, changePercent)

		// 更新内存中的 initialBalance
		at.initialBalance = actualBalance

		// 更新数据库（需要类型断言）
		if at.database != nil {
			// 这里需要根据实际的数据库类型进行类型断言
			// 由于使用了 interface{}，我们需要在 TraderManager 层面处理更新
			// 或者在这里进行类型检查
			type DatabaseUpdater interface {
				UpdateTraderInitialBalance(userID, id string, newBalance float64) error
			}
			if db, ok := at.database.(DatabaseUpdater); ok {
				err := db.UpdateTraderInitialBalance(at.userID, at.id, actualBalance)
				if err != nil {
					log.Printf("❌ [%s] 更新数据库失败: %v", at.name, err)
				} else {
					log.Printf("✅ [%s] 已自动同步余额到数据库", at.name)
				}
			} else {
				log.Printf("⚠️ [%s] 数据库类型不支持UpdateTraderInitialBalance接口", at.name)
			}
		} else {
			log.Printf("⚠️ [%s] 数据库引用为空，余额仅在内存中更新", at.name)
		}
	} else {
		log.Printf("✓ [%s] 余额变化不大 (%.2f%%)，无需更新", at.name, changePercent)
	}

	at.lastBalanceSyncTime = time.Now()
}

// runCycle 运行一个交易周期（使用AI全权决策）
func (at *AutoTrader) runCycle() error {
	at.callCount++

	log.Print("\n" + strings.Repeat("=", 70) + "\n")
	log.Printf("⏰ %s - AI决策周期 #%d", time.Now().Format("2006-01-02 15:04:05"), at.callCount)
	log.Println(strings.Repeat("=", 70))

	// 创建决策记录
	record := &logger.DecisionRecord{
		ExecutionLog: []string{},
		Success:      true,
	}

	// 1. 检查是否需要停止交易
	if time.Now().Before(at.stopUntil) {
		remaining := at.stopUntil.Sub(time.Now())
		log.Printf("⏸ 风险控制：暂停交易中，剩余 %.0f 分钟", remaining.Minutes())
		record.Success = false
		record.ErrorMessage = fmt.Sprintf("风险控制暂停中，剩余 %.0f 分钟", remaining.Minutes())
		at.decisionLogger.LogDecision(record)
		return nil
	}

	// 2. 重置日盈亏（每天重置）
	if time.Since(at.lastResetTime) > 24*time.Hour {
		at.dailyPnL = 0
		at.lastResetTime = time.Now()
		log.Println("📅 日盈亏已重置")
	}

	// 3. 自动同步余额（每10分钟检查一次，充值/提现后自动更新）
	at.autoSyncBalanceIfNeeded()

	// 4. 收集交易上下文
	ctx, err := at.buildTradingContext()
	if err != nil {
		record.Success = false
		record.ErrorMessage = fmt.Sprintf("构建交易上下文失败: %v", err)
		at.decisionLogger.LogDecision(record)
		return fmt.Errorf("构建交易上下文失败: %w", err)
	}

	// 保存账户状态快照
	record.AccountState = logger.AccountSnapshot{
		TotalBalance:          ctx.Account.TotalEquity,
		AvailableBalance:      ctx.Account.AvailableBalance,
		TotalUnrealizedProfit: ctx.Account.TotalPnL,
		PositionCount:         ctx.Account.PositionCount,
		MarginUsedPct:         ctx.Account.MarginUsedPct,
	}

	// 保存持仓快照
	for _, pos := range ctx.Positions {
		record.Positions = append(record.Positions, logger.PositionSnapshot{
			Symbol:           pos.Symbol,
			Side:             pos.Side,
			PositionAmt:      pos.Quantity,
			EntryPrice:       pos.EntryPrice,
			MarkPrice:        pos.MarkPrice,
			UnrealizedProfit: pos.UnrealizedPnL,
			Leverage:         float64(pos.Leverage),
			LiquidationPrice: pos.LiquidationPrice,
		})
	}

	log.Print(strings.Repeat("=", 70))
	for _, coin := range ctx.CandidateCoins {
		record.CandidateCoins = append(record.CandidateCoins, coin.Symbol)
	}

	log.Printf("📊 账户净值: %.2f USDT | 可用: %.2f USDT | 持仓: %d",
		ctx.Account.TotalEquity, ctx.Account.AvailableBalance, ctx.Account.PositionCount)

	// 5. 调用AI获取完整决策
	log.Printf("🤖 正在请求AI分析并决策... [模板: %s]", at.systemPromptTemplate)
	decision, err := decision.GetFullDecisionWithCustomPrompt(ctx, at.mcpClient, at.customPrompt, at.overrideBasePrompt, at.systemPromptTemplate)

	// 即使有错误，也保存思维链、决策和输入prompt（用于debug）
	if decision != nil {
		record.SystemPrompt = decision.SystemPrompt // 保存系统提示词
		record.InputPrompt = decision.UserPrompt
		record.CoTTrace = decision.CoTTrace
		if len(decision.Decisions) > 0 {
			decisionJSON, _ := json.MarshalIndent(decision.Decisions, "", "  ")
			record.DecisionJSON = string(decisionJSON)
		}
	}

	if err != nil {
		record.Success = false
		record.ErrorMessage = fmt.Sprintf("获取AI决策失败: %v", err)

		// 打印系统提示词和AI思维链（即使有错误，也要输出以便调试）
		if decision != nil {
			log.Print("\n" + strings.Repeat("=", 70) + "\n")
			log.Printf("📋 系统提示词 [模板: %s] (错误情况)", at.systemPromptTemplate)
			log.Println(strings.Repeat("=", 70))
			log.Println(decision.SystemPrompt)
			log.Println(strings.Repeat("=", 70))

			if decision.CoTTrace != "" {
				log.Print("\n" + strings.Repeat("-", 70) + "\n")
				log.Println("💭 AI思维链分析（错误情况）:")
				log.Println(strings.Repeat("-", 70))
				log.Println(decision.CoTTrace)
				log.Println(strings.Repeat("-", 70))
			}
		}

		at.decisionLogger.LogDecision(record)
		return fmt.Errorf("获取AI决策失败: %w", err)
	}

	// // 5. 打印系统提示词
	// log.Printf("\n" + strings.Repeat("=", 70))
	// log.Printf("📋 系统提示词 [模板: %s]", at.systemPromptTemplate)
	// log.Println(strings.Repeat("=", 70))
	// log.Println(decision.SystemPrompt)
	// log.Printf(strings.Repeat("=", 70) + "\n")

	// 6. 打印AI思维链
	// log.Printf("\n" + strings.Repeat("-", 70))
	// log.Println("💭 AI思维链分析:")
	// log.Println(strings.Repeat("-", 70))
	// log.Println(decision.CoTTrace)
	// log.Printf(strings.Repeat("-", 70) + "\n")

	// 7. 打印AI决策
	// log.Printf("📋 AI决策列表 (%d 个):\n", len(decision.Decisions))
	// for i, d := range decision.Decisions {
	//     log.Printf("  [%d] %s: %s - %s", i+1, d.Symbol, d.Action, d.Reasoning)
	//     if d.Action == "open_long" || d.Action == "open_short" {
	//        log.Printf("      杠杆: %dx | 仓位: %.2f USDT | 止损: %.4f | 止盈: %.4f",
	//           d.Leverage, d.PositionSizeUSD, d.StopLoss, d.TakeProfit)
	//     }
	// }
	// 7. 基于现价补齐/矫正 TP/SL（缺失或 R:R<3 时回退 0.8%/0.2%）
	at.applyFallbackTPAndSL(decision.Decisions)

	// 更新记录的决策快照（包含兜底后的 TP/SL）
	if marshaled, err := json.MarshalIndent(decision.Decisions, "", "  "); err == nil {
		record.DecisionJSON = string(marshaled)
	}

	log.Println()
	log.Print(strings.Repeat("-", 70))
	// 8. 自动补充动态止盈指令（若AI未覆盖且浮盈达到阈值）
	autoTPDecisions := at.buildAutoTakeProfitDecisions(ctx, decision.Decisions)
	if len(autoTPDecisions) > 0 {
		log.Printf("⚙️ 自动补充 %d 个动态止盈指令，确保盈利单同步调整目标", len(autoTPDecisions))
		decision.Decisions = append(decision.Decisions, autoTPDecisions...)
	}

	autoSLDecisions := at.buildAutoStopLossDecisions(ctx, decision.Decisions)
	if len(autoSLDecisions) > 0 {
		log.Printf("⚠️ 结构/动能失效，自动追加 %d 条收紧止损指令", len(autoSLDecisions))
		decision.Decisions = append(decision.Decisions, autoSLDecisions...)
	}

	if len(autoTPDecisions) > 0 || len(autoSLDecisions) > 0 {
		if marshaled, err := json.MarshalIndent(decision.Decisions, "", "  "); err == nil {
			record.DecisionJSON = string(marshaled)
		}
	}

	// 9. 对决策排序：确保先平仓后开仓（防止仓位叠加超限）
	log.Print(strings.Repeat("-", 70))
	sortedDecisions := sortDecisionsByPriority(decision.Decisions)

	log.Println("🔄 执行顺序（已优化）: 先平仓→后开仓")
	for i, d := range sortedDecisions {
		log.Printf("  [%d] %s %s", i+1, d.Symbol, d.Action)
	}
	log.Println()

	// 执行决策并记录结果
	for _, d := range sortedDecisions {
		actionRecord := logger.DecisionAction{
			Action:    d.Action,
			Symbol:    d.Symbol,
			Quantity:  0,
			Leverage:  d.Leverage,
			Price:     0,
			Timestamp: time.Now(),
			Success:   false,
		}

		if err := at.executeDecisionWithRecord(&d, &actionRecord); err != nil {
			log.Printf("❌ 执行决策失败 (%s %s): %v", d.Symbol, d.Action, err)
			actionRecord.Error = err.Error()
			record.ExecutionLog = append(record.ExecutionLog, fmt.Sprintf("❌ %s %s 失败: %v", d.Symbol, d.Action, err))
		} else {
			actionRecord.Success = true
			record.ExecutionLog = append(record.ExecutionLog, fmt.Sprintf("✓ %s %s 成功", d.Symbol, d.Action))
			// 成功执行后短暂延迟
			time.Sleep(1 * time.Second)
		}

		record.Decisions = append(record.Decisions, actionRecord)
	}

	// 9. 保存决策记录
	if err := at.decisionLogger.LogDecision(record); err != nil {
		log.Printf("⚠ 保存决策记录失败: %v", err)
	}

	return nil
}

// buildTradingContext 构建交易上下文
func (at *AutoTrader) buildTradingContext() (*decision.Context, error) {
	// 1. 获取账户信息
	balance, err := at.trader.GetBalance()
	if err != nil {
		return nil, fmt.Errorf("获取账户余额失败: %w", err)
	}

	// 获取账户字段
	totalWalletBalance := 0.0
	totalUnrealizedProfit := 0.0
	availableBalance := 0.0

	if wallet, ok := balance["totalWalletBalance"].(float64); ok {
		totalWalletBalance = wallet
	}
	if unrealized, ok := balance["totalUnrealizedProfit"].(float64); ok {
		totalUnrealizedProfit = unrealized
	}
	if avail, ok := balance["availableBalance"].(float64); ok {
		availableBalance = avail
	}

	// Total Equity = 钱包余额 + 未实现盈亏
	totalEquity := totalWalletBalance + totalUnrealizedProfit

	// 2. 获取持仓信息
	positions, err := at.trader.GetPositions()
	if err != nil {
		return nil, fmt.Errorf("获取持仓失败: %w", err)
	}

	var positionInfos []decision.PositionInfo
	totalMarginUsed := 0.0

	// 当前持仓的key集合（用于清理已平仓的记录）
	currentPositionKeys := make(map[string]bool)

	for _, pos := range positions {
		symbol := pos["symbol"].(string)
		side := pos["side"].(string)
		entryPrice := pos["entryPrice"].(float64)
		markPrice := pos["markPrice"].(float64)
		quantity := pos["positionAmt"].(float64)
		if quantity < 0 {
			quantity = -quantity // 空仓数量为负，转为正数
		}

		// 跳过已平仓的持仓（quantity = 0），防止"幽灵持仓"传递给AI
		if quantity == 0 {
			continue
		}

		unrealizedPnl := pos["unRealizedProfit"].(float64)
		liquidationPrice := pos["liquidationPrice"].(float64)

		// 计算盈亏百分比
		pnlPct := 0.0
		if side == "long" {
			pnlPct = ((markPrice - entryPrice) / entryPrice) * 100
		} else {
			pnlPct = ((entryPrice - markPrice) / entryPrice) * 100
		}

		// 计算占用保证金（估算）
		leverage := 10 // 默认值，实际应该从持仓信息获取
		if lev, ok := pos["leverage"].(float64); ok {
			leverage = int(lev)
		}
		marginUsed := (quantity * markPrice) / float64(leverage)
		totalMarginUsed += marginUsed

		// 跟踪持仓首次出现时间
		posKey := at.positionKey(symbol, side)
		currentPositionKeys[posKey] = true
		if _, exists := at.positionFirstSeenTime[posKey]; !exists {
			// 新持仓，记录当前时间
			at.positionFirstSeenTime[posKey] = time.Now().UnixMilli()
		}
		updateTime := at.positionFirstSeenTime[posKey]

		positionInfos = append(positionInfos, decision.PositionInfo{
			Symbol:           symbol,
			Side:             side,
			EntryPrice:       entryPrice,
			MarkPrice:        markPrice,
			Quantity:         quantity,
			Leverage:         leverage,
			UnrealizedPnL:    unrealizedPnl,
			UnrealizedPnLPct: pnlPct,
			LiquidationPrice: liquidationPrice,
			MarginUsed:       marginUsed,
			UpdateTime:       updateTime,
		})
	}

	// 清理已平仓的持仓记录
	for key := range at.positionFirstSeenTime {
		if !currentPositionKeys[key] {
			delete(at.positionFirstSeenTime, key)
			at.stopUpdateMutex.Lock()
			delete(at.lastStopUpdate, key)
			at.stopUpdateMutex.Unlock()
		}
	}

	// 3. 获取交易员的候选币种池
	candidateCoins, err := at.getCandidateCoins()
	if err != nil {
		return nil, fmt.Errorf("获取候选币种失败: %w", err)
	}

	// 4. 计算总盈亏
	totalPnL := totalEquity - at.initialBalance
	totalPnLPct := 0.0
	if at.initialBalance > 0 {
		totalPnLPct = (totalPnL / at.initialBalance) * 100
	}

	marginUsedPct := 0.0
	if totalEquity > 0 {
		marginUsedPct = (totalMarginUsed / totalEquity) * 100
	}

	// 5. 分析历史表现（最近100个周期，避免长期持仓的交易记录丢失）
	// 假设每3分钟一个周期，100个周期 = 5小时，足够覆盖大部分交易
	performance, err := at.decisionLogger.AnalyzePerformance(100)
	if err != nil {
		log.Printf("⚠️  分析历史表现失败: %v", err)
		// 不影响主流程，继续执行（但设置performance为nil以避免传递错误数据）
		performance = nil
	}

	// 6. 构建上下文
	ctx := &decision.Context{
		CurrentTime:     time.Now().Format("2006-01-02 15:04:05"),
		RuntimeMinutes:  int(time.Since(at.startTime).Minutes()),
		CallCount:       at.callCount,
		BTCETHLeverage:  at.config.BTCETHLeverage,  // 使用配置的杠杆倍数
		AltcoinLeverage: at.config.AltcoinLeverage, // 使用配置的杠杆倍数
		Account: decision.AccountInfo{
			TotalEquity:      totalEquity,
			AvailableBalance: availableBalance,
			TotalPnL:         totalPnL,
			TotalPnLPct:      totalPnLPct,
			MarginUsed:       totalMarginUsed,
			MarginUsedPct:    marginUsedPct,
			PositionCount:    len(positionInfos),
		},
		Positions:      positionInfos,
		CandidateCoins: candidateCoins,
		Performance:    performance, // 添加历史表现分析
	}

	return ctx, nil
}

// executeDecisionWithRecord 执行AI决策并记录详细信息
func (at *AutoTrader) executeDecisionWithRecord(decision *decision.Decision, actionRecord *logger.DecisionAction) error {
	switch decision.Action {
	case "open_long":
		return at.executeOpenLongWithRecord(decision, actionRecord)
	case "open_short":
		return at.executeOpenShortWithRecord(decision, actionRecord)
	case "close_long":
		return at.executeCloseLongWithRecord(decision, actionRecord)
	case "close_short":
		return at.executeCloseShortWithRecord(decision, actionRecord)
	case "update_stop_loss":
		return at.executeUpdateStopLossWithRecord(decision, actionRecord)
	case "update_take_profit":
		return at.executeUpdateTakeProfitWithRecord(decision, actionRecord)
	case "partial_close":
		return at.executePartialCloseWithRecord(decision, actionRecord)
	case "hold", "wait":
		// 无需执行，仅记录
		return nil
	default:
		return fmt.Errorf("未知的action: %s", decision.Action)
	}
}

func ensureMidTermEntryFilters(data *market.Data, direction string, relax bool) error {
	if data == nil || data.MidTermContext == nil {
		return fmt.Errorf("缺少15m指标，无法验证%s条件", direction)
	}

	mt := data.MidTermContext
	if mt.ATR14 <= 0 || mt.EMA20 == 0 || mt.RSI7 <= 0 {
		return fmt.Errorf("15m指标尚未就绪，拒绝%s", direction)
	}

	price := data.CurrentPrice
	switch direction {
	case "long":
		upper := mt.EMA20 + 0.6*mt.ATR14
		if mt.RSI7 > 68 && !relax {
			return fmt.Errorf("15m RSI(7)=%.2f 超出68，提示词要求 wait", mt.RSI7)
		}
		if price > upper && !relax {
			return fmt.Errorf("价格 %.2f 高于 15m EMA20+0.6ATR(%.2f)，拒绝追多", price, upper)
		}
		if relax && mt.RSI7 > 75 {
			return fmt.Errorf("15m RSI(7)=%.2f 已严重超买，即便单边行情也禁止追多", mt.RSI7)
		}
	case "short":
		lower := mt.EMA20 - 0.6*mt.ATR14
		if mt.RSI7 < 32 && !relax {
			return fmt.Errorf("15m RSI(7)=%.2f 低于32，提示词要求 wait", mt.RSI7)
		}
		if price < lower && !relax {
			return fmt.Errorf("价格 %.2f 低于 15m EMA20-0.6ATR(%.2f)，拒绝追空", price, lower)
		}
		if relax && mt.RSI7 < 25 {
			return fmt.Errorf("15m RSI(7)=%.2f 已极度超卖，放宽策略仍不允许继续追空", mt.RSI7)
		}
	default:
		return fmt.Errorf("未知方向: %s", direction)
	}

	return nil
}

// ensureShortTermMomentum 要求 3m 指标确认方向（防追价/逆势）
func ensureShortTermMomentum(data *market.Data, direction string, relax bool) error {
	if data == nil {
		return fmt.Errorf("缺少短周期指标，无法验证%s条件", direction)
	}

	macd := data.CurrentMACD
	switch direction {
	case "long":
		if macd < -floatEpsilon && !relax {
			return fmt.Errorf("3m MACD=%.2f 仍为负值，短线动能未转多", macd)
		}
		if relax && macd < -0.5 {
			return fmt.Errorf("3m MACD=%.2f 仍显著偏空，放宽策略也无法追多", macd)
		}
	case "short":
		if macd > floatEpsilon && !relax {
			return fmt.Errorf("3m MACD=%.2f 仍为正值，短线动能未转空", macd)
		}
		if relax && macd > 0.5 {
			return fmt.Errorf("3m MACD=%.2f 仍显著偏多，放宽策略也无法追空", macd)
		}
	default:
		return fmt.Errorf("未知方向: %s", direction)
	}

	if series := data.IntradaySeries; series != nil && len(series.RSI7Values) >= 2 {
		last := series.RSI7Values[len(series.RSI7Values)-1]
		prev := series.RSI7Values[len(series.RSI7Values)-2]
		slope := last - prev
		if direction == "long" && slope < -0.2 && !relax {
			return fmt.Errorf("3m RSI 未出现回升确认 (%.2f→%.2f)", prev, last)
		}
		if direction == "short" && slope > 0.2 && !relax {
			return fmt.Errorf("3m RSI 未出现回落确认 (%.2f→%.2f)", prev, last)
		}
		if relax {
			if direction == "long" && slope < -0.5 {
				return fmt.Errorf("3m RSI 仍急剧走低 (%.2f→%.2f)，即使放宽也无法确认反弹", prev, last)
			}
			if direction == "short" && slope > 0.5 {
				return fmt.Errorf("3m RSI 快速回升 (%.2f→%.2f)，即使放宽也禁止继续追空", prev, last)
			}
		}
	}

	return nil
}

// shouldRelaxEntryFilters 结合 1h/4h 价格变化与 ATR 扩张度，判断是否允许放宽“不可追价”限制
func (at *AutoTrader) shouldRelaxEntryFilters(data *market.Data, direction string) (bool, string) {
	if data == nil || data.MidTermContext == nil || data.LongerTermContext == nil {
		return false, ""
	}

	if !envBool(envRelaxEnabled, true) {
		return false, ""
	}

	mt := data.MidTermContext
	lt := data.LongerTermContext
	if mt.ATR14 <= 0 || lt.ATR14 <= 0 {
		return false, ""
	}

	atrRatio := 0.0
	if lt.ATR14 > 0 {
		atrRatio = lt.ATR3 / lt.ATR14
	}
	priceDistance := math.Abs(data.CurrentPrice-mt.EMA20) / mt.ATR14
	oneHour := data.PriceChange1h
	fourHour := data.PriceChange4h

	switch strings.ToLower(direction) {
	case "short":
		strongDown := fourHour <= -0.4 && oneHour <= -0.3
		if strongDown && atrRatio >= 0.95 && priceDistance >= 0.6 {
			return true, fmt.Sprintf("4h%.2f%%/1h%.2f%% 共振下跌且 ATR 扩张%.2f", fourHour, oneHour, atrRatio)
		}
	case "long":
		rebound := fourHour <= -0.3 && oneHour >= 0.5
		trendingUp := fourHour >= 0.5 && oneHour >= 0.4
		if (rebound || trendingUp) && atrRatio >= 0.95 && priceDistance >= 0.5 {
			return true, fmt.Sprintf("1h 强劲拉升(%.2f%%) 配合 ATR 扩张%.2f", oneHour, atrRatio)
		}
	}

	return false, ""
}

func isMajorPair(symbol string) bool {
	s := strings.ToUpper(symbol)
	return s == "BTCUSDT" || s == "ETHUSDT"
}

// applyFallbackTPAndSL 在 TP/SL 缺失或基于现价的实际 R:R < 3 时，回退为固定 0.8% / 0.2% 的止盈止损
func (at *AutoTrader) applyFallbackTPAndSL(decisions []decision.Decision) {
	for i := range decisions {
		d := &decisions[i]
		if d.Action != "open_long" && d.Action != "open_short" {
			continue
		}

		marketData, err := market.Get(d.Symbol)
		if err != nil || marketData == nil || marketData.CurrentPrice <= 0 {
			log.Printf("⚠️ 获取 %s 行情失败，跳过 TP/SL 兜底: %v", d.Symbol, err)
			continue
		}
		currentPrice := marketData.CurrentPrice

		needFallback := false
		if d.StopLoss <= 0 || d.TakeProfit <= 0 {
			needFallback = true
		} else {
			var risk, reward float64
			if d.Action == "open_long" {
				risk = currentPrice - d.StopLoss
				reward = d.TakeProfit - currentPrice
			} else {
				risk = d.StopLoss - currentPrice
				reward = currentPrice - d.TakeProfit
			}
			if risk <= 0 || reward <= 0 || reward/risk < 3.0 {
				needFallback = true
			}
		}

		if !needFallback {
			continue
		}

		// 计算 ATR 下限，避免兜底止损小于噪音
		minStopDistance := 0.0
		if marketData.MidTermContext != nil && marketData.MidTermContext.ATR14 > 0 {
			minStopDistance = 0.5 * marketData.MidTermContext.ATR14
		}

		if d.Action == "open_long" {
			d.StopLoss = currentPrice * (1 - fallbackStopLossPct)
			d.TakeProfit = currentPrice * (1 + fallbackTakeProfitPct)
		} else {
			d.StopLoss = currentPrice * (1 + fallbackStopLossPct)
			d.TakeProfit = currentPrice * (1 - fallbackTakeProfitPct)
		}

		// 若 ATR 下限大于百分比止损，则用 ATR 下限替代
		if minStopDistance > 0 {
			if d.Action == "open_long" {
				d.StopLoss = math.Min(d.StopLoss, currentPrice-minStopDistance)
			} else {
				d.StopLoss = math.Max(d.StopLoss, currentPrice+minStopDistance)
			}
		}

		d.Reasoning = strings.TrimSpace(fmt.Sprintf(
			"%s | [fallback] TP/SL 回退为 %.2f%% / %.2f%% (现价 %.4f)，SL≥0.5×ATR14(1h) — 原始参数缺失或实际 R:R < 3",
			d.Reasoning, fallbackTakeProfitPct*100, fallbackStopLossPct*100, currentPrice))
	}
}

func (at *AutoTrader) ensurePositionFitsBalance(decision *decision.Decision, availableBalance, totalEquity float64, marketData *market.Data) error {
	if decision.Leverage <= 0 {
		return fmt.Errorf("杠杆未设置，无法计算保证金")
	}

	// 预留 0.5U 或 2% 余额作为手续费缓冲
	safetyBuffer := math.Max(0.5, availableBalance*0.02)
	maxUsable := availableBalance - safetyBuffer
	if maxUsable <= 0 {
		return fmt.Errorf("可用余额 %.2f USDT 不足以覆盖手续费缓冲", availableBalance)
	}

	maxNotional := maxUsable * float64(decision.Leverage)
	minNotional := 12.0
	if isMajorPair(decision.Symbol) {
		minNotional = 60.0
	}
	if envBool(envMinNotionalGuard, true) {
		minNotional = math.Max(minNotional, exchangeMinNotionalU99*1.1) // 给 10% 缓冲，避免浮点/精度导致再次触发 notional 错误
	}
	at.ensureMinPositionNotional(decision, minNotional)

	if maxNotional < minNotional {
		return fmt.Errorf("可用余额仅支撑 %.2f USDT 名义价值，低于最小下单要求 %.2f USDT", maxNotional, minNotional)
	}

	maxRisk := availableBalance * 0.8

	if marketData == nil {
		return fmt.Errorf("需要行情数据以验证止损距离")
	}
	allowed, err := allowedStopDistance(marketData)
	if err != nil || allowed <= 0 {
		return fmt.Errorf("缺少 ATR14(1h) 数据，无法验证止损距离: %v", err)
	}
	distance := math.Abs(decision.StopLoss - marketData.CurrentPrice)
	if distance > allowed {
		if !at.clampStopLossWithinATR(decision, marketData.CurrentPrice, allowed) {
			return fmt.Errorf("初始止损距离 %.2f 超出允许 %.2f (≈1×ATR14 1h)，请重新计算止损或仓位", distance, allowed)
		}
		distance = math.Abs(decision.StopLoss - marketData.CurrentPrice)
	}

	if totalEquity > floatEpsilon {
		softMultiplier := 1.5
		if isMajorPair(decision.Symbol) {
			softMultiplier = 3.0
		}
		softCap := totalEquity * softMultiplier
		if softCap > floatEpsilon && decision.PositionSizeUSD > softCap {
			ratio := softCap / decision.PositionSizeUSD
			log.Printf("  🪢 标准模式软性仓位上限: %s %.2f → %.2f USDT (净值%.2f×%.1f)", decision.Symbol, decision.PositionSizeUSD, softCap, totalEquity, softMultiplier)
			decision.PositionSizeUSD = softCap
			if decision.RiskUSD > 0 {
				decision.RiskUSD *= ratio
			}
		}
	}

	if decision.PositionSizeUSD > maxNotional {
		ratio := maxNotional / decision.PositionSizeUSD
		log.Printf("  ⚖️ 自动下调 %s 仓位: %.2f → %.2f USDT (余额%.2f)", decision.Symbol, decision.PositionSizeUSD, maxNotional, availableBalance)
		decision.PositionSizeUSD = maxNotional
		if decision.RiskUSD > 0 {
			decision.RiskUSD *= ratio
		}
	}

	// 最终防呆：软/硬上限可能把仓位压到最小名义以下，此时宁可报错也不送到交易所
	if envBool(envMinNotionalGuard, true) && decision.PositionSizeUSD+floatEpsilon < minNotional {
		return fmt.Errorf("名义价值 %.2f USDT 低于最小下单要求 %.2f USDT，放弃下单以避免交易所拒单", decision.PositionSizeUSD, minNotional)
	}

	if decision.RiskUSD > 0 && decision.RiskUSD > maxRisk {
		log.Printf("  ⚠️ 风险预算 %.2f USDT 超过余额 80%% (%.2f)，自动降至 %.2f", decision.RiskUSD, maxRisk, maxRisk)
		decision.RiskUSD = maxRisk
	}

	// risk_usd 与净值挂钩（提示词约定 0.5%），避免 AI 输出与执行层不一致
	if totalEquity > floatEpsilon {
		expectedRisk := math.Max(totalEquity*0.005, 0.5)
		if decision.RiskUSD <= 0 {
			decision.RiskUSD = expectedRisk
		} else {
			diff := math.Abs(decision.RiskUSD - expectedRisk)
			if diff > expectedRisk*0.5 {
				log.Printf("  ⚠️ 调整 %s 风险预算: %.2f → %.2f (依据净值 %.2f)", decision.Symbol, decision.RiskUSD, expectedRisk, totalEquity)
				decision.RiskUSD = expectedRisk
			}
		}
	}

	return nil
}

func allowedStopDistance(data *market.Data) (float64, error) {
	if data == nil {
		return 0, fmt.Errorf("缺少行情数据，无法计算 ATR")
	}

	if data.LongerTermContext != nil && data.LongerTermContext.ATR14 > 0 {
		return data.LongerTermContext.ATR14, nil
	}

	candidates := []float64{}
	if data.MidTermContext != nil && data.MidTermContext.ATR14 > 0 {
		candidates = append(candidates, data.MidTermContext.ATR14*1.5)
	}
	if data.CurrentPrice > 0 {
		candidates = append(candidates, data.CurrentPrice*0.015)
	}

	allowed := 0.0
	for _, c := range candidates {
		if c > allowed {
			allowed = c
		}
	}

	if allowed <= 0 {
		return 0, fmt.Errorf("无法计算止损距离（ATR缺失）")
	}
	return allowed, nil
}

func (at *AutoTrader) clampStopLossWithinATR(decision *decision.Decision, price, allowed float64) bool {
	if price <= 0 || allowed <= 0 {
		return false
	}
	distance := math.Abs(decision.StopLoss - price)
	if distance <= allowed+floatEpsilon {
		return true
	}

	buffer := allowed * 0.98
	if buffer <= 0 {
		buffer = allowed
	}

	isLong := decision.StopLoss < price
	var newStop float64
	if isLong {
		newStop = price - buffer
		if newStop <= 0 {
			return false
		}
	} else {
		newStop = price + buffer
	}

	log.Printf("  📏 调整 %s 初始止损距: %.2f → %.2f (允许 %.2f)", decision.Symbol, decision.StopLoss, newStop, allowed)
	decision.StopLoss = newStop
	return math.Abs(decision.StopLoss-price) <= allowed+floatEpsilon
}

func (at *AutoTrader) ensureMinPositionNotional(decision *decision.Decision, minNotional float64) {
	if minNotional <= 0 {
		return
	}
	if decision.PositionSizeUSD+floatEpsilon >= minNotional {
		return
	}
	log.Printf("  ⬆️ 自动提升 %s 名义价值以满足最小下单: %.2f → %.2f USDT", decision.Symbol, decision.PositionSizeUSD, minNotional)
	decision.PositionSizeUSD = minNotional
}

// executeOpenLongWithRecord 执行开多仓并记录详细信息
func (at *AutoTrader) executeOpenLongWithRecord(decision *decision.Decision, actionRecord *logger.DecisionAction) error {
	log.Printf("  📈 开多仓: %s", decision.Symbol)

	if decision.Confidence < minConfidence {
		// 开仓前硬限制置信度，避免低质量信号落地
		return fmt.Errorf("置信度不足 (%d < %d)，拒绝开多仓", decision.Confidence, minConfidence)
	}

	// ⚠️ 关键：检查是否已有同币种同方向持仓，如果有则拒绝开仓（防止仓位叠加超限）
	positions, err := at.trader.GetPositions()
	if err == nil {
		for _, pos := range positions {
			if pos["symbol"] == decision.Symbol && pos["side"] == "long" {
				return fmt.Errorf("❌ %s 已有多仓，拒绝开仓以防止仓位叠加超限。如需换仓，请先给出 close_long 决策", decision.Symbol)
			}
		}
	}

	// 获取当前价格
	marketData, err := market.Get(decision.Symbol)
	if err != nil {
		return err
	}
	relaxFilters, relaxReason := at.shouldRelaxEntryFilters(marketData, "long")
	if relaxFilters && relaxReason != "" {
		log.Printf("  ⚡ 启用顺势追涨模式: %s", relaxReason)
	}
	if err := ensureMidTermEntryFilters(marketData, "long", relaxFilters); err != nil {
		return err
	}
	if err := ensureShortTermMomentum(marketData, "long", relaxFilters); err != nil {
		return err
	}

	// ⚠️ 保证金验证：防止保证金不足错误（code=-2019）
	balance, err := at.trader.GetBalance()
	if err != nil {
		return fmt.Errorf("获取账户余额失败: %w", err)
	}
	availableBalance := 0.0
	if avail, ok := balance["availableBalance"].(float64); ok {
		availableBalance = avail
	}

	totalEquity := availableBalance
	if total, ok := balance["totalWalletBalance"].(float64); ok && total > 0 {
		totalEquity = total
		if unrealized, ok := balance["totalUnrealizedProfit"].(float64); ok {
			totalEquity += unrealized
		}
	}

	if err := at.ensurePositionFitsBalance(decision, availableBalance, totalEquity, marketData); err != nil {
		return err
	}

	// 计算数量（使用可能被调整后的仓位规模）
	quantity := decision.PositionSizeUSD / marketData.CurrentPrice
	actionRecord.Quantity = quantity
	actionRecord.Price = marketData.CurrentPrice

	requiredMargin := decision.PositionSizeUSD / float64(decision.Leverage)
	// 手续费估算（Taker费率 0.04%）
	estimatedFee := decision.PositionSizeUSD * 0.0004
	totalRequired := requiredMargin + estimatedFee

	if totalRequired > availableBalance {
		return fmt.Errorf("❌ 保证金不足: 需要 %.2f USDT（保证金 %.2f + 手续费 %.2f），可用 %.2f USDT",
			totalRequired, requiredMargin, estimatedFee, availableBalance)
	}

	// 设置仓位模式
	if err := at.trader.SetMarginMode(decision.Symbol, at.config.IsCrossMargin); err != nil {
		log.Printf("  ⚠️ 设置仓位模式失败: %v", err)
		// 继续执行，不影响交易
	}

	// 开仓
	order, err := at.trader.OpenLong(decision.Symbol, quantity, decision.Leverage)
	if err != nil {
		return err
	}

	// 记录订单ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	log.Printf("  ✓ 开仓成功，订单ID: %v, 数量: %.4f", order["orderId"], quantity)

	// 缓存入场细节，供后续盈亏/止损逻辑使用
	at.storePositionMeta(decision.Symbol, "long", marketData.CurrentPrice, quantity)

	// 记录开仓时间
	posKey := at.positionKey(decision.Symbol, "long")
	at.positionFirstSeenTime[posKey] = time.Now().UnixMilli()

	// 设置止损止盈
	if err := at.trader.SetStopLoss(decision.Symbol, "LONG", quantity, decision.StopLoss); err != nil {
		log.Printf("  ⚠ 设置止损失败: %v", err)
	} else {
		// 初次成功设置止损后同步更新缓存
		at.storeStopLoss(decision.Symbol, "long", decision.StopLoss)
	}
	if err := at.trader.SetTakeProfit(decision.Symbol, "LONG", quantity, decision.TakeProfit); err != nil {
		log.Printf("  ⚠ 设置止盈失败: %v", err)
	} else {
		at.storeTakeProfit(decision.Symbol, "long", decision.TakeProfit)
	}

	return nil
}

// executeOpenShortWithRecord 执行开空仓并记录详细信息
func (at *AutoTrader) executeOpenShortWithRecord(decision *decision.Decision, actionRecord *logger.DecisionAction) error {
	log.Printf("  📉 开空仓: %s", decision.Symbol)

	if decision.Confidence < minConfidence {
		// 置信度不达标的空单直接拦截
		return fmt.Errorf("置信度不足 (%d < %d)，拒绝开空仓", decision.Confidence, minConfidence)
	}

	// ⚠️ 关键：检查是否已有同币种同方向持仓，如果有则拒绝开仓（防止仓位叠加超限）
	positions, err := at.trader.GetPositions()
	if err == nil {
		for _, pos := range positions {
			if pos["symbol"] == decision.Symbol && pos["side"] == "short" {
				return fmt.Errorf("❌ %s 已有空仓，拒绝开仓以防止仓位叠加超限。如需换仓，请先给出 close_short 决策", decision.Symbol)
			}
		}
	}

	// 获取当前价格
	marketData, err := market.Get(decision.Symbol)
	if err != nil {
		return err
	}
	relaxFilters, relaxReason := at.shouldRelaxEntryFilters(marketData, "short")
	if relaxFilters && relaxReason != "" {
		log.Printf("  ⚡ 启用顺势追空模式: %s", relaxReason)
	}
	if err := ensureMidTermEntryFilters(marketData, "short", relaxFilters); err != nil {
		return err
	}
	if err := ensureShortTermMomentum(marketData, "short", relaxFilters); err != nil {
		return err
	}

	// ⚠️ 保证金验证：防止保证金不足错误（code=-2019）
	balance, err := at.trader.GetBalance()
	if err != nil {
		return fmt.Errorf("获取账户余额失败: %w", err)
	}
	availableBalance := 0.0
	if avail, ok := balance["availableBalance"].(float64); ok {
		availableBalance = avail
	}

	totalEquity := availableBalance
	if total, ok := balance["totalWalletBalance"].(float64); ok && total > 0 {
		totalEquity = total
		if unrealized, ok := balance["totalUnrealizedProfit"].(float64); ok {
			totalEquity += unrealized
		}
	}

	if err := at.ensurePositionFitsBalance(decision, availableBalance, totalEquity, marketData); err != nil {
		return err
	}

	// 计算数量（使用可能被调整后的仓位规模）
	quantity := decision.PositionSizeUSD / marketData.CurrentPrice
	actionRecord.Quantity = quantity
	actionRecord.Price = marketData.CurrentPrice

	requiredMargin := decision.PositionSizeUSD / float64(decision.Leverage)
	// 手续费估算（Taker费率 0.04%）
	estimatedFee := decision.PositionSizeUSD * 0.0004
	totalRequired := requiredMargin + estimatedFee

	if totalRequired > availableBalance {
		return fmt.Errorf("❌ 保证金不足: 需要 %.2f USDT（保证金 %.2f + 手续费 %.2f），可用 %.2f USDT",
			totalRequired, requiredMargin, estimatedFee, availableBalance)
	}

	// 设置仓位模式
	if err := at.trader.SetMarginMode(decision.Symbol, at.config.IsCrossMargin); err != nil {
		log.Printf("  ⚠️ 设置仓位模式失败: %v", err)
		// 继续执行，不影响交易
	}

	// 开仓
	order, err := at.trader.OpenShort(decision.Symbol, quantity, decision.Leverage)
	if err != nil {
		return err
	}

	// 记录订单ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	log.Printf("  ✓ 开仓成功，订单ID: %v, 数量: %.4f", order["orderId"], quantity)

	// 保存空头入场信息，为后续止损/盈利统计做准备
	at.storePositionMeta(decision.Symbol, "short", marketData.CurrentPrice, quantity)

	// 记录开仓时间
	posKey := at.positionKey(decision.Symbol, "short")
	at.positionFirstSeenTime[posKey] = time.Now().UnixMilli()

	// 设置止损止盈
	if err := at.trader.SetStopLoss(decision.Symbol, "SHORT", quantity, decision.StopLoss); err != nil {
		log.Printf("  ⚠ 设置止损失败: %v", err)
	} else {
		// 记录初始止损，确保之后只能向盈亏方向移动
		at.storeStopLoss(decision.Symbol, "short", decision.StopLoss)
	}
	if err := at.trader.SetTakeProfit(decision.Symbol, "SHORT", quantity, decision.TakeProfit); err != nil {
		log.Printf("  ⚠ 设置止盈失败: %v", err)
	} else {
		at.storeTakeProfit(decision.Symbol, "short", decision.TakeProfit)
	}

	return nil
}

// executeCloseLongWithRecord 执行平多仓并记录详细信息
func (at *AutoTrader) executeCloseLongWithRecord(decision *decision.Decision, actionRecord *logger.DecisionAction) error {
	log.Printf("  🔄 平多仓: %s", decision.Symbol)

	// 获取当前价格
	marketData, err := market.Get(decision.Symbol)
	if err != nil {
		return err
	}
	actionRecord.Price = marketData.CurrentPrice

	// 尝试从缓存读取完整持仓数量（用于盈亏统计）
	closedQty := at.getPositionQuantity(decision.Symbol, "long")
	if closedQty > 0 {
		actionRecord.Quantity = closedQty
	}

	// 平仓
	order, err := at.trader.CloseLong(decision.Symbol, 0) // 0 = 全部平仓
	if err != nil {
		return err
	}

	// 记录订单ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	log.Printf("  ✓ 平仓成功")
	at.handleRealizedPnL(decision.Symbol, "long", closedQty, marketData.CurrentPrice)
	return nil
}

// executeCloseShortWithRecord 执行平空仓并记录详细信息
func (at *AutoTrader) executeCloseShortWithRecord(decision *decision.Decision, actionRecord *logger.DecisionAction) error {
	log.Printf("  🔄 平空仓: %s", decision.Symbol)

	// 获取当前价格
	marketData, err := market.Get(decision.Symbol)
	if err != nil {
		return err
	}
	actionRecord.Price = marketData.CurrentPrice

	// 空头同样读取缓存数量
	closedQty := at.getPositionQuantity(decision.Symbol, "short")
	if closedQty > 0 {
		actionRecord.Quantity = closedQty
	}

	// 平仓
	order, err := at.trader.CloseShort(decision.Symbol, 0) // 0 = 全部平仓
	if err != nil {
		return err
	}

	// 记录订单ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	log.Printf("  ✓ 平仓成功")
	at.handleRealizedPnL(decision.Symbol, "short", closedQty, marketData.CurrentPrice)
	return nil
}

// executeUpdateStopLossWithRecord 执行调整止损并记录详细信息
func (at *AutoTrader) executeUpdateStopLossWithRecord(decision *decision.Decision, actionRecord *logger.DecisionAction) error {
	log.Printf("  🎯 调整止损: %s → %.2f", decision.Symbol, decision.NewStopLoss)

	// 获取当前价格
	marketData, err := market.Get(decision.Symbol)
	if err != nil {
		return err
	}
	actionRecord.Price = marketData.CurrentPrice

	// 获取当前持仓
	positions, err := at.trader.GetPositions()
	if err != nil {
		return fmt.Errorf("获取持仓失败: %w", err)
	}

	// 查找目标持仓
	var targetPosition map[string]interface{}
	for _, pos := range positions {
		symbol, _ := pos["symbol"].(string)
		posAmt, _ := pos["positionAmt"].(float64)
		if symbol == decision.Symbol && posAmt != 0 {
			targetPosition = pos
			break
		}
	}

	if targetPosition == nil {
		return fmt.Errorf("持仓不存在: %s", decision.Symbol)
	}

	// 获取持仓方向和数量
	side, _ := targetPosition["side"].(string)
	positionSide := strings.ToUpper(side)
	normalizedSide := strings.ToLower(positionSide)
	positionAmt, _ := targetPosition["positionAmt"].(float64)

	// 验证新止损价格合理性
	if positionSide == "LONG" && decision.NewStopLoss >= marketData.CurrentPrice {
		return fmt.Errorf("多单止损必须低于当前价格 (当前: %.2f, 新止损: %.2f)", marketData.CurrentPrice, decision.NewStopLoss)
	}
	if positionSide == "SHORT" && decision.NewStopLoss <= marketData.CurrentPrice {
		return fmt.Errorf("空单止损必须高于当前价格 (当前: %.2f, 新止损: %.2f)", marketData.CurrentPrice, decision.NewStopLoss)
	}

	if err := at.ensureStopLossTightening(decision.Symbol, positionSide, decision.NewStopLoss); err != nil {
		return err
	}

	entryPrice, _ := targetPosition["entryPrice"].(float64)
	leverage := 1
	if lev, ok := targetPosition["leverage"].(float64); ok && lev > 0 {
		leverage = int(math.Round(lev))
	}
	atr := 0.0
	if marketData.MidTermContext != nil {
		atr = marketData.MidTermContext.ATR14
	}
	if err := at.validateStopTightening(decision.Symbol, normalizedSide, entryPrice, marketData.CurrentPrice, decision.NewStopLoss, atr, leverage); err != nil {
		return err
	}

	// 取消旧的止损单（避免多个止损单共存）
	if err := at.trader.CancelStopOrders(decision.Symbol); err != nil {
		log.Printf("  ⚠ 取消旧止损单失败: %v", err)
		// 不中断执行，继续设置新止损
	}

	// 调用交易所 API 修改止损
	quantity := math.Abs(positionAmt)
	err = at.trader.SetStopLoss(decision.Symbol, positionSide, quantity, decision.NewStopLoss)
	if err != nil {
		return fmt.Errorf("修改止损失败: %w", err)
	}

	at.storeStopLoss(decision.Symbol, positionSide, decision.NewStopLoss)
	at.markStopUpdate(decision.Symbol, normalizedSide)

	log.Printf("  ✓ 止损已调整: %.2f (当前价格: %.2f)", decision.NewStopLoss, marketData.CurrentPrice)
	return nil
}

// executeUpdateTakeProfitWithRecord 执行调整止盈并记录详细信息
func (at *AutoTrader) executeUpdateTakeProfitWithRecord(decision *decision.Decision, actionRecord *logger.DecisionAction) error {
	log.Printf("  🎯 调整止盈: %s → %.2f", decision.Symbol, decision.NewTakeProfit)

	// 获取当前价格
	marketData, err := market.Get(decision.Symbol)
	if err != nil {
		return err
	}
	actionRecord.Price = marketData.CurrentPrice

	// 获取当前持仓
	positions, err := at.trader.GetPositions()
	if err != nil {
		return fmt.Errorf("获取持仓失败: %w", err)
	}

	// 查找目标持仓
	var targetPosition map[string]interface{}
	for _, pos := range positions {
		symbol, _ := pos["symbol"].(string)
		posAmt, _ := pos["positionAmt"].(float64)
		if symbol == decision.Symbol && posAmt != 0 {
			targetPosition = pos
			break
		}
	}

	if targetPosition == nil {
		return fmt.Errorf("持仓不存在: %s", decision.Symbol)
	}

	// 获取持仓方向和数量
	side, _ := targetPosition["side"].(string)
	positionSide := strings.ToUpper(side)
	positionAmt, _ := targetPosition["positionAmt"].(float64)

	// 验证新止盈价格合理性
	if positionSide == "LONG" && decision.NewTakeProfit <= marketData.CurrentPrice {
		return fmt.Errorf("多单止盈必须高于当前价格 (当前: %.2f, 新止盈: %.2f)", marketData.CurrentPrice, decision.NewTakeProfit)
	}
	if positionSide == "SHORT" && decision.NewTakeProfit >= marketData.CurrentPrice {
		return fmt.Errorf("空单止盈必须低于当前价格 (当前: %.2f, 新止盈: %.2f)", marketData.CurrentPrice, decision.NewTakeProfit)
	}

	// 取消旧的止盈单（避免多个止盈单共存）
	if err := at.trader.CancelStopOrders(decision.Symbol); err != nil {
		log.Printf("  ⚠ 取消旧止盈单失败: %v", err)
		// 不中断执行，继续设置新止盈
	}

	// 调用交易所 API 修改止盈
	quantity := math.Abs(positionAmt)
	err = at.trader.SetTakeProfit(decision.Symbol, positionSide, quantity, decision.NewTakeProfit)
	if err != nil {
		return fmt.Errorf("修改止盈失败: %w", err)
	}

	at.storeTakeProfit(decision.Symbol, positionSide, decision.NewTakeProfit)

	log.Printf("  ✓ 止盈已调整: %.2f (当前价格: %.2f)", decision.NewTakeProfit, marketData.CurrentPrice)
	return nil
}

// executePartialCloseWithRecord 执行部分平仓并记录详细信息
func (at *AutoTrader) executePartialCloseWithRecord(decision *decision.Decision, actionRecord *logger.DecisionAction) error {
	log.Printf("  📊 部分平仓: %s %.1f%%", decision.Symbol, decision.ClosePercentage)

	// 验证百分比范围
	if decision.ClosePercentage <= 0 || decision.ClosePercentage > 100 {
		return fmt.Errorf("平仓百分比必须在 0-100 之间，当前: %.1f", decision.ClosePercentage)
	}

	// 获取当前价格
	marketData, err := market.Get(decision.Symbol)
	if err != nil {
		return err
	}
	actionRecord.Price = marketData.CurrentPrice

	// 获取当前持仓
	positions, err := at.trader.GetPositions()
	if err != nil {
		return fmt.Errorf("获取持仓失败: %w", err)
	}

	// 查找目标持仓
	var targetPosition map[string]interface{}
	for _, pos := range positions {
		symbol, _ := pos["symbol"].(string)
		posAmt, _ := pos["positionAmt"].(float64)
		if symbol == decision.Symbol && posAmt != 0 {
			targetPosition = pos
			break
		}
	}

	if targetPosition == nil {
		return fmt.Errorf("持仓不存在: %s", decision.Symbol)
	}

	// 获取持仓方向和数量
	side, _ := targetPosition["side"].(string)
	positionSide := strings.ToUpper(side)
	positionAmt, _ := targetPosition["positionAmt"].(float64)

	// 计算平仓数量
	totalQuantity := math.Abs(positionAmt)
	closeQuantity := totalQuantity * (decision.ClosePercentage / 100.0)
	minQty := at.estimateStepSize(decision.Symbol)
	if closeQuantity < minQty {
		if totalQuantity <= minQty+floatEpsilon {
			closeQuantity = totalQuantity
		} else {
			log.Printf("  ⚠️ %s 部分平仓数量 %.6f 低于最小步长 %.6f，按最小值执行", decision.Symbol, closeQuantity, minQty)
			closeQuantity = minQty
		}
	}
	closeQuantity = at.roundQuantity(decision.Symbol, closeQuantity)
	if closeQuantity <= 0 {
		return fmt.Errorf("平仓数量过小，无法执行（步长 %.6f）", minQty)
	}
	actionRecord.Quantity = closeQuantity

	// 执行平仓
	var order map[string]interface{}
	if positionSide == "LONG" {
		order, err = at.trader.CloseLong(decision.Symbol, closeQuantity)
	} else {
		order, err = at.trader.CloseShort(decision.Symbol, closeQuantity)
	}

	if err != nil {
		return fmt.Errorf("部分平仓失败: %w", err)
	}

	// 记录订单ID
	if orderID, ok := order["orderId"].(int64); ok {
		actionRecord.OrderID = orderID
	}

	remainingQuantity := totalQuantity - closeQuantity
	log.Printf("  ✓ 部分平仓成功: 平仓 %.4f (%.1f%%), 剩余 %.4f",
		closeQuantity, decision.ClosePercentage, remainingQuantity)

	at.handleRealizedPnL(decision.Symbol, strings.ToLower(positionSide), closeQuantity, marketData.CurrentPrice)
	at.evaluateSymbolProtection(decision.Symbol)

	return nil
}

// GetID 获取trader ID
func (at *AutoTrader) GetID() string {
	return at.id
}

// GetName 获取trader名称
func (at *AutoTrader) GetName() string {
	return at.name
}

// GetAIModel 获取AI模型
func (at *AutoTrader) GetAIModel() string {
	return at.aiModel
}

// GetExchange 获取交易所
func (at *AutoTrader) GetExchange() string {
	return at.exchange
}

// SetCustomPrompt 设置自定义交易策略prompt
func (at *AutoTrader) SetCustomPrompt(prompt string) {
	at.customPrompt = prompt
}

// SetOverrideBasePrompt 设置是否覆盖基础prompt
func (at *AutoTrader) SetOverrideBasePrompt(override bool) {
	at.overrideBasePrompt = override
}

// SetSystemPromptTemplate 设置系统提示词模板
func (at *AutoTrader) SetSystemPromptTemplate(templateName string) {
	at.systemPromptTemplate = templateName
}

// GetSystemPromptTemplate 获取当前系统提示词模板名称
func (at *AutoTrader) GetSystemPromptTemplate() string {
	return at.systemPromptTemplate
}

// GetDecisionLogger 获取决策日志记录器
func (at *AutoTrader) GetDecisionLogger() *logger.DecisionLogger {
	return at.decisionLogger
}

// GetStatus 获取系统状态（用于API）
func (at *AutoTrader) GetStatus() map[string]interface{} {
	aiProvider := "DeepSeek"
	if at.config.UseQwen {
		aiProvider = "Qwen"
	}

	return map[string]interface{}{
		"trader_id":       at.id,
		"trader_name":     at.name,
		"ai_model":        at.aiModel,
		"exchange":        at.exchange,
		"is_running":      at.isRunning,
		"start_time":      at.startTime.Format(time.RFC3339),
		"runtime_minutes": int(time.Since(at.startTime).Minutes()),
		"call_count":      at.callCount,
		"initial_balance": at.initialBalance,
		"scan_interval":   at.config.ScanInterval.String(),
		"stop_until":      at.stopUntil.Format(time.RFC3339),
		"last_reset_time": at.lastResetTime.Format(time.RFC3339),
		"ai_provider":     aiProvider,
	}
}

// GetAccountInfo 获取账户信息（用于API）
func (at *AutoTrader) GetAccountInfo() (map[string]interface{}, error) {
	balance, err := at.trader.GetBalance()
	if err != nil {
		return nil, fmt.Errorf("获取余额失败: %w", err)
	}

	// 获取账户字段
	totalWalletBalance := 0.0
	totalUnrealizedProfit := 0.0
	availableBalance := 0.0

	if wallet, ok := balance["totalWalletBalance"].(float64); ok {
		totalWalletBalance = wallet
	}
	if unrealized, ok := balance["totalUnrealizedProfit"].(float64); ok {
		totalUnrealizedProfit = unrealized
	}
	if avail, ok := balance["availableBalance"].(float64); ok {
		availableBalance = avail
	}

	// Total Equity = 钱包余额 + 未实现盈亏
	totalEquity := totalWalletBalance + totalUnrealizedProfit

	// 获取持仓计算总保证金
	positions, err := at.trader.GetPositions()
	if err != nil {
		return nil, fmt.Errorf("获取持仓失败: %w", err)
	}

	totalMarginUsed := 0.0
	totalUnrealizedPnL := 0.0
	for _, pos := range positions {
		markPrice := pos["markPrice"].(float64)
		quantity := pos["positionAmt"].(float64)
		if quantity < 0 {
			quantity = -quantity
		}
		unrealizedPnl := pos["unRealizedProfit"].(float64)
		totalUnrealizedPnL += unrealizedPnl

		leverage := 10
		if lev, ok := pos["leverage"].(float64); ok {
			leverage = int(lev)
		}
		marginUsed := (quantity * markPrice) / float64(leverage)
		totalMarginUsed += marginUsed
	}

	totalPnL := totalEquity - at.initialBalance
	totalPnLPct := 0.0
	if at.initialBalance > 0 {
		totalPnLPct = (totalPnL / at.initialBalance) * 100
	}

	marginUsedPct := 0.0
	if totalEquity > 0 {
		marginUsedPct = (totalMarginUsed / totalEquity) * 100
	}

	return map[string]interface{}{
		// 核心字段
		"total_equity":      totalEquity,           // 账户净值 = wallet + unrealized
		"wallet_balance":    totalWalletBalance,    // 钱包余额（不含未实现盈亏）
		"unrealized_profit": totalUnrealizedProfit, // 未实现盈亏（从API）
		"available_balance": availableBalance,      // 可用余额

		// 盈亏统计
		"total_pnl":            totalPnL,           // 总盈亏 = equity - initial
		"total_pnl_pct":        totalPnLPct,        // 总盈亏百分比
		"total_unrealized_pnl": totalUnrealizedPnL, // 未实现盈亏（从持仓计算）
		"initial_balance":      at.initialBalance,  // 初始余额
		"daily_pnl":            at.dailyPnL,        // 日盈亏

		// 持仓信息
		"position_count":  len(positions),  // 持仓数量
		"margin_used":     totalMarginUsed, // 保证金占用
		"margin_used_pct": marginUsedPct,   // 保证金使用率
	}, nil
}

// GetPositions 获取持仓列表（用于API）
func (at *AutoTrader) GetPositions() ([]map[string]interface{}, error) {
	positions, err := at.trader.GetPositions()
	if err != nil {
		return nil, fmt.Errorf("获取持仓失败: %w", err)
	}

	var result []map[string]interface{}
	for _, pos := range positions {
		symbol := pos["symbol"].(string)
		side := pos["side"].(string)
		entryPrice := pos["entryPrice"].(float64)
		markPrice := pos["markPrice"].(float64)
		quantity := pos["positionAmt"].(float64)
		if quantity < 0 {
			quantity = -quantity
		}
		unrealizedPnl := pos["unRealizedProfit"].(float64)
		liquidationPrice := pos["liquidationPrice"].(float64)

		leverage := 10
		if lev, ok := pos["leverage"].(float64); ok {
			leverage = int(lev)
		}

		// 计算占用保证金
		marginUsed := (quantity * markPrice) / float64(leverage)

		// 计算盈亏百分比（基于保证金）
		// 收益率 = 未实现盈亏 / 保证金 × 100%
		pnlPct := 0.0
		if marginUsed > 0 {
			pnlPct = (unrealizedPnl / marginUsed) * 100
		}

		result = append(result, map[string]interface{}{
			"symbol":             symbol,
			"side":               side,
			"entry_price":        entryPrice,
			"mark_price":         markPrice,
			"quantity":           quantity,
			"leverage":           leverage,
			"unrealized_pnl":     unrealizedPnl,
			"unrealized_pnl_pct": pnlPct,
			"liquidation_price":  liquidationPrice,
			"margin_used":        marginUsed,
		})
	}

	return result, nil
}

// sortDecisionsByPriority 对决策排序：先平仓，再开仓，最后hold/wait
// 这样可以避免换仓时仓位叠加超限
func sortDecisionsByPriority(decisions []decision.Decision) []decision.Decision {
	if len(decisions) <= 1 {
		return decisions
	}

	// 定义优先级
	getActionPriority := func(action string) int {
		switch action {
		case "close_long", "close_short", "partial_close":
			return 1 // 最高优先级：先平仓（包括部分平仓）
		case "update_stop_loss", "update_take_profit":
			return 2 // 调整持仓止盈止损
		case "open_long", "open_short":
			return 3 // 次优先级：后开仓
		case "hold", "wait":
			return 4 // 最低优先级：观望
		default:
			return 999 // 未知动作放最后
		}
	}

	// 复制决策列表
	sorted := make([]decision.Decision, len(decisions))
	copy(sorted, decisions)

	// 按优先级排序
	for i := 0; i < len(sorted)-1; i++ {
		for j := i + 1; j < len(sorted); j++ {
			if getActionPriority(sorted[i].Action) > getActionPriority(sorted[j].Action) {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	return sorted
}

func findPositionSide(positions []decision.PositionInfo, symbol string) string {
	for _, pos := range positions {
		if strings.EqualFold(pos.Symbol, symbol) && pos.Quantity > 0 {
			return strings.ToLower(pos.Side)
		}
	}
	return ""
}

func (at *AutoTrader) buildAutoTakeProfitDecisions(ctx *decision.Context, base []decision.Decision) []decision.Decision {
	if ctx == nil {
		return nil
	}

	existing := make(map[string]bool)
	for _, d := range base {
		if d.Action != "update_take_profit" {
			continue
		}
		side := findPositionSide(ctx.Positions, d.Symbol)
		if side == "" {
			side = "long"
		}
		key := strings.ToUpper(d.Symbol) + "_" + side
		existing[key] = true
	}

	var autoDecisions []decision.Decision
	for _, pos := range ctx.Positions {
		side := strings.ToLower(pos.Side)
		key := strings.ToUpper(pos.Symbol) + "_" + side
		if existing[key] {
			continue
		}

		stop, ok := at.getStopLoss(pos.Symbol, side)
		if !ok {
			continue
		}

		entry := pos.EntryPrice
		var risk float64
		if side == "long" {
			risk = entry - stop
		} else {
			risk = stop - entry
		}
		if risk <= floatEpsilon {
			continue
		}

		currentPrice := pos.MarkPrice
		if ctx.MarketDataMap != nil {
			if data, ok := ctx.MarketDataMap[pos.Symbol]; ok && data.CurrentPrice > 0 {
				currentPrice = data.CurrentPrice
			}
		}

		var favorable float64
		if side == "long" {
			favorable = currentPrice - entry
		} else {
			favorable = entry - currentPrice
		}
		if favorable <= risk {
			continue
		}

		rMultiple := favorable / risk
		var targetMultiple float64
		switch {
		case rMultiple >= 3.0:
			targetMultiple = rMultiple + 0.5
		case rMultiple >= 2.0:
			targetMultiple = 3.5
		case rMultiple >= 1.5:
			targetMultiple = 2.5
		default:
			continue
		}

		var desiredTP float64
		if side == "long" {
			desiredTP = entry + targetMultiple*risk
			if desiredTP <= currentPrice {
				desiredTP = currentPrice + 0.3*risk
			}
		} else {
			desiredTP = entry - targetMultiple*risk
			if desiredTP >= currentPrice {
				desiredTP = currentPrice - 0.3*risk
			}
		}

		if tp, ok := at.getTakeProfit(pos.Symbol, side); ok {
			if side == "long" && desiredTP <= tp*(1+1e-5) {
				continue
			}
			if side == "short" && desiredTP >= tp*(1-1e-5) {
				continue
			}
		}

		autoDecisions = append(autoDecisions, decision.Decision{
			Symbol:        pos.Symbol,
			Action:        "update_take_profit",
			NewTakeProfit: desiredTP,
			Reasoning:     fmt.Sprintf("[auto_tp] 浮盈%.2fR，自动上调止盈追踪趋势", rMultiple),
		})
		existing[key] = true
	}

	return autoDecisions
}

func (at *AutoTrader) buildAutoStopLossDecisions(ctx *decision.Context, base []decision.Decision) []decision.Decision {
	if ctx == nil || len(ctx.Positions) == 0 {
		return nil
	}

	existing := make(map[string]bool)
	for _, d := range base {
		if d.Action == "update_stop_loss" {
			key := strings.ToUpper(d.Symbol)
			existing[key] = true
		}
	}

	var out []decision.Decision
	for _, pos := range ctx.Positions {
		side := strings.ToLower(pos.Side)
		key := strings.ToUpper(pos.Symbol)
		if existing[key] {
			continue
		}

		entry := pos.EntryPrice
		current := pos.MarkPrice
		if ctx.MarketDataMap != nil {
			if data, ok := ctx.MarketDataMap[pos.Symbol]; ok && data != nil && data.CurrentPrice > 0 {
				current = data.CurrentPrice
			}
		}

		stop, ok := at.getStopLoss(pos.Symbol, side)
		if !ok || stop == 0 {
			continue
		}

		var risk, adverse float64
		if side == "long" {
			risk = entry - stop
			adverse = entry - current
		} else {
			risk = stop - entry
			adverse = current - entry
		}

		if risk <= floatEpsilon {
			continue
		}

		var rsi float64
		if ctx.MarketDataMap != nil {
			if data, ok := ctx.MarketDataMap[pos.Symbol]; ok && data != nil && data.MidTermContext != nil {
				rsi = data.MidTermContext.RSI7
			}
		}

		trigger := false
		if side == "short" {
			if current > entry || adverse >= 0.4*risk || rsi >= 55 {
				trigger = true
			}
		} else {
			if current < entry || adverse >= 0.4*risk || (rsi > 0 && rsi <= 45) {
				trigger = true
			}
		}

		if !trigger {
			continue
		}

		var newStop float64
		buffer := math.Max(current*0.0008, risk*0.15)
		if side == "short" {
			newStop = current + buffer
			if newStop >= stop {
				newStop = stop - buffer*0.5
			}
			if newStop <= current {
				newStop = current + buffer
			}
			if newStop >= stop-floatEpsilon {
				continue
			}
		} else {
			newStop = current - buffer
			if newStop <= stop {
				newStop = stop + buffer*0.5
			}
			if newStop >= current {
				newStop = current - buffer
			}
			if newStop <= stop+floatEpsilon {
				continue
			}
		}

		out = append(out, decision.Decision{
			Symbol:      pos.Symbol,
			Action:      "update_stop_loss",
			NewStopLoss: newStop,
			Reasoning:   "结构/动能失效，自动收紧止损保护本金",
		})
		existing[key] = true
	}

	return out
}

// getCandidateCoins 获取交易员的候选币种列表
func (at *AutoTrader) getCandidateCoins() ([]decision.CandidateCoin, error) {
	if len(at.tradingCoins) == 0 {
		// 使用数据库配置的默认币种列表
		var candidateCoins []decision.CandidateCoin

		if len(at.defaultCoins) > 0 {
			// 使用数据库中配置的默认币种
			for _, coin := range at.defaultCoins {
				symbol := normalizeSymbol(coin)
				candidateCoins = append(candidateCoins, decision.CandidateCoin{
					Symbol:  symbol,
					Sources: []string{"default"}, // 标记为数据库默认币种
				})
			}
			log.Printf("📋 [%s] 使用数据库默认币种: %d个币种 %v",
				at.name, len(candidateCoins), at.defaultCoins)
			return candidateCoins, nil
		} else {
			// 如果数据库中没有配置默认币种，则使用AI500+OI Top作为fallback
			const ai500Limit = 20 // AI500取前20个评分最高的币种

			mergedPool, err := pool.GetMergedCoinPool(ai500Limit)
			if err != nil {
				return nil, fmt.Errorf("获取合并币种池失败: %w", err)
			}

			// 构建候选币种列表（包含来源信息）
			for _, symbol := range mergedPool.AllSymbols {
				sources := mergedPool.SymbolSources[symbol]
				candidateCoins = append(candidateCoins, decision.CandidateCoin{
					Symbol:  symbol,
					Sources: sources, // "ai500" 和/或 "oi_top"
				})
			}

			log.Printf("📋 [%s] 数据库无默认币种配置，使用AI500+OI Top: AI500前%d + OI_Top20 = 总计%d个候选币种",
				at.name, ai500Limit, len(candidateCoins))
			return candidateCoins, nil
		}
	} else {
		// 使用自定义币种列表
		var candidateCoins []decision.CandidateCoin
		for _, coin := range at.tradingCoins {
			// 确保币种格式正确（转为大写USDT交易对）
			symbol := normalizeSymbol(coin)
			candidateCoins = append(candidateCoins, decision.CandidateCoin{
				Symbol:  symbol,
				Sources: []string{"custom"}, // 标记为自定义来源
			})
		}

		log.Printf("📋 [%s] 使用自定义币种: %d个币种 %v",
			at.name, len(candidateCoins), at.tradingCoins)
		return candidateCoins, nil
	}
}

// normalizeSymbol 标准化币种符号（确保以USDT结尾）
func normalizeSymbol(symbol string) string {
	// 转为大写
	symbol = strings.ToUpper(strings.TrimSpace(symbol))

	// 确保以USDT结尾
	if !strings.HasSuffix(symbol, "USDT") {
		symbol = symbol + "USDT"
	}

	return symbol
}

type roiProfile struct {
	breakeven float64
	lock30    float64
	lock50    float64
	drawdown  float64
	floor     float64
}

// roiProfileFor 返回不同杠杆下的 ROI 锁盈/回撤阈值
func roiProfileFor(leverage int) roiProfile {
	switch {
	case leverage <= 2:
		return roiProfile{breakeven: 6, lock30: 8, lock50: 12, drawdown: 40, floor: 0.7}
	case leverage <= 5:
		return roiProfile{breakeven: 3, lock30: 6, lock50: 10, drawdown: 35, floor: 2}
	case leverage <= 10:
		return roiProfile{breakeven: 2, lock30: 4, lock50: 7, drawdown: 30, floor: 3}
	default:
		return roiProfile{breakeven: 1.5, lock30: 3, lock50: 5, drawdown: 25, floor: 3.5}
	}
}

// autoProtectionBuffer 返回更新止损时的最小缓冲，兼顾 tick size 与波动
func (at *AutoTrader) autoProtectionBuffer(markPrice, atr float64) float64 {
	buffer := markPrice * 0.0002
	if atr > 0 {
		buffer = math.Max(buffer, atr*0.05)
	}
	if buffer < 0.01 {
		buffer = 0.01
	}
	return buffer
}

// pickTighterStop 选出“更紧”的止损（多单向上、空单向下），保持单调收敛
func (at *AutoTrader) pickTighterStop(side string, current float64, hasCurrent bool, candidate float64) (float64, bool) {
	if math.IsNaN(candidate) {
		return current, hasCurrent
	}
	if !hasCurrent {
		return candidate, true
	}
	if strings.ToLower(side) == "long" {
		if candidate > current+floatEpsilon {
			return candidate, true
		}
	} else {
		if candidate < current-floatEpsilon {
			return candidate, true
		}
	}
	return current, hasCurrent
}

// positionRoiPct 基于入场价/行情价估算单仓 ROI%
func (at *AutoTrader) positionRoiPct(side string, entryPrice, markPrice float64, leverage int) float64 {
	if leverage <= 0 {
		leverage = 1
	}
	if strings.ToLower(side) == "long" {
		return ((markPrice - entryPrice) / entryPrice) * float64(leverage) * 100
	}
	return ((entryPrice - markPrice) / entryPrice) * float64(leverage) * 100
}

// atrStopCandidate 根据 ATR 档位给出新的止损候选
func (at *AutoTrader) atrStopCandidate(side string, entryPrice, markPrice, gain float64, data *market.Data) (float64, bool) {
	if data == nil || data.MidTermContext == nil || data.MidTermContext.ATR14 <= 0 {
		return 0, false
	}
	atr := data.MidTermContext.ATR14
	if gain <= 0 {
		return 0, false
	}

	var target float64
	hasCandidate := false
	if gain >= atr {
		target = entryPrice
		hasCandidate = true
	}
	if gain >= 1.5*atr {
		if strings.ToLower(side) == "long" {
			target = entryPrice + 0.5*atr
		} else {
			target = entryPrice - 0.5*atr
		}
		hasCandidate = true
	}
	if gain >= 2*atr {
		if strings.ToLower(side) == "long" {
			target = markPrice - 2.5*atr
		} else {
			target = markPrice + 2.5*atr
		}
		hasCandidate = true
	}
	if !hasCandidate {
		return 0, false
	}
	buffer := at.autoProtectionBuffer(markPrice, atr)
	if strings.ToLower(side) == "long" {
		target = math.Min(target, markPrice-buffer)
	}
	if strings.ToLower(side) == "short" {
		target = math.Max(target, markPrice+buffer)
	}
	return target, true
}

// roiStopCandidate 根据 ROI 阶梯返回锁盈价格（叠加保底收益）
func (at *AutoTrader) roiStopCandidate(side string, entryPrice, markPrice float64, leverage int, roiPct, gain float64) (float64, bool) {
	profile := roiProfileFor(leverage)
	if roiPct < profile.breakeven {
		return 0, false
	}

	if gain <= 0 {
		return 0, false
	}

	var candidate float64
	if strings.ToLower(side) == "long" {
		candidate = entryPrice
	} else {
		candidate = entryPrice
	}

	if roiPct >= profile.lock30 {
		if strings.ToLower(side) == "long" {
			candidate = entryPrice + gain*0.3
		} else {
			candidate = entryPrice - gain*0.3
		}
	}
	if roiPct >= profile.lock50 {
		if strings.ToLower(side) == "long" {
			candidate = entryPrice + gain*0.5
		} else {
			candidate = entryPrice - gain*0.5
		}
	}

	if profile.floor > 0 {
		floorGain := (profile.floor / 100.0) / float64(leverage)
		if strings.ToLower(side) == "long" {
			minStop := entryPrice * (1 + floorGain)
			candidate = math.Max(candidate, minStop)
		} else {
			minStop := entryPrice * (1 - floorGain)
			candidate = math.Min(candidate, minStop)
		}
	}

	buffer := at.autoProtectionBuffer(markPrice, gain)
	if strings.ToLower(side) == "long" && candidate > markPrice-buffer {
		candidate = markPrice - buffer
	}
	if strings.ToLower(side) == "short" && candidate < markPrice+buffer {
		candidate = markPrice + buffer
	}
	return candidate, true
}

// shouldBypassHoldDuration 当 ROI 达到倍数阈值时允许忽略最小持仓时长
func (at *AutoTrader) shouldBypassHoldDuration(roi float64) bool {
	if at.config.AllowImmediateTighteningROI == nil || !*at.config.AllowImmediateTighteningROI {
		return false
	}
	if at.config.MinStopAdjustROI <= 0 {
		return false
	}
	multiple := at.config.ImmediateTighteningROIMultiple
	if multiple <= 0 {
		return false
	}
	return roi >= at.config.MinStopAdjustROI*multiple
}

// positionHoldDuration 返回该仓位已持有时间
func (at *AutoTrader) positionHoldDuration(symbol, side string) time.Duration {
	key := at.positionKey(symbol, side)
	if firstSeen, ok := at.positionFirstSeenTime[key]; ok && firstSeen > 0 {
		return time.Since(time.UnixMilli(firstSeen))
	}
	return at.config.MinStopAdjustHold
}

func (at *AutoTrader) timeSinceLastStopUpdate(symbol, side string) time.Duration {
	key := at.positionKey(symbol, side)
	at.stopUpdateMutex.RLock()
	last, ok := at.lastStopUpdate[key]
	at.stopUpdateMutex.RUnlock()
	if !ok {
		return at.config.MinStopUpdateInterval
	}
	return time.Since(last)
}

func (at *AutoTrader) markStopUpdate(symbol, side string) {
	key := at.positionKey(symbol, side)
	at.stopUpdateMutex.Lock()
	at.lastStopUpdate[key] = time.Now()
	at.stopUpdateMutex.Unlock()
}

func (at *AutoTrader) clearStopUpdateTimestamp(symbol, side string) {
	key := at.positionKey(symbol, side)
	at.stopUpdateMutex.Lock()
	delete(at.lastStopUpdate, key)
	at.stopUpdateMutex.Unlock()
}

// validateStopTightening 执行层统一校验止损收紧的节流规则
func (at *AutoTrader) validateStopTightening(symbol, side string, entryPrice, currentPrice, newStop, atr float64, leverage int) error {
	minROI := at.config.MinStopAdjustROI
	roi := at.positionRoiPct(side, entryPrice, currentPrice, leverage)
	if minROI > 0 && roi+floatEpsilon < minROI {
		return fmt.Errorf("ROI %.2f%% 未达到最小阈值 %.2f%%", roi, minROI)
	}

	hold := at.positionHoldDuration(symbol, side)
	if at.config.MinStopAdjustHold > 0 && hold < at.config.MinStopAdjustHold && !at.shouldBypassHoldDuration(roi) {
		return fmt.Errorf("持仓仅 %s，未达到最小持仓时长 %s", hold.Truncate(time.Second), at.config.MinStopAdjustHold)
	}

	if at.config.MinStopUpdateInterval > 0 {
		elapsed := at.timeSinceLastStopUpdate(symbol, side)
		if elapsed < at.config.MinStopUpdateInterval {
			return fmt.Errorf("距离上次收紧仅过去 %s (< %s)", elapsed.Truncate(time.Second), at.config.MinStopUpdateInterval)
		}
	}

	if atr > 0 && at.config.MinStopAdjustATRs > 0 {
		minDistance := atr * at.config.MinStopAdjustATRs
		distance := math.Abs(entryPrice - newStop)
		if distance+floatEpsilon < minDistance {
			return fmt.Errorf("止损需至少距离入场 %.2f (ATR×%.2f)，当前 %.2f", minDistance, at.config.MinStopAdjustATRs, distance)
		}
	}

	return nil
}

// floatingGain 计算多/空持仓的正向浮盈（亏损则返回0）
func floatingGain(side string, entryPrice, markPrice float64) float64 {
	if strings.EqualFold(side, "long") {
		if markPrice > entryPrice {
			return markPrice - entryPrice
		}
		return 0
	}
	if entryPrice > markPrice {
		return entryPrice - markPrice
	}
	return 0
}

// dispatchAutoStopLoss 组装零延迟的 update_stop_loss 决策
func (at *AutoTrader) dispatchAutoStopLoss(symbol, side string, newStop float64, reason string) {
	dec := &decision.Decision{
		Symbol:      symbol,
		Action:      "update_stop_loss",
		NewStopLoss: newStop,
		Reasoning:   reason,
	}
	action := &logger.DecisionAction{
		Action: "update_stop_loss",
		Symbol: symbol,
	}
	if err := at.executeUpdateStopLossWithRecord(dec, action); err != nil {
		log.Printf("⚠️ 自动锁盈更新 %s 失败: %v", symbol, err)
	} else {
		log.Printf("🔒 自动锁盈已收紧 %s (%s) → %.4f", symbol, side, newStop)
	}
}

// applyDynamicProtection 综合 ATR/ROI 规则，必要时自动追踪止损（始终优先通过 update_stop_loss 来保护仓位）
func (at *AutoTrader) applyDynamicProtection(pos map[string]interface{}) {
	symbol, _ := pos["symbol"].(string)
	side, _ := pos["side"].(string)
	entryPrice, _ := pos["entryPrice"].(float64)
	markPrice, _ := pos["markPrice"].(float64)
	positionAmt, _ := pos["positionAmt"].(float64)

	if symbol == "" || side == "" || math.Abs(positionAmt) < floatEpsilon {
		return
	}

	leverage := 1
	if lev, ok := pos["leverage"].(float64); ok && lev > 0 {
		leverage = int(math.Round(lev))
		if leverage <= 0 {
			leverage = 1
		}
	}

	gain := floatingGain(side, entryPrice, markPrice)
	if gain <= floatEpsilon {
		return // 浮盈未达到，暂不收紧止损
	}

	marketData, err := market.Get(symbol)
	if err != nil {
		log.Printf("⚠️ 自动锁盈获取行情失败(%s): %v", symbol, err)
		return
	}

	currentStop, hasStop := at.getStopLoss(symbol, side)
	targetStop := currentStop
	targetExists := hasStop

	if atrCandidate, ok := at.atrStopCandidate(side, entryPrice, markPrice, gain, marketData); ok {
		targetStop, targetExists = at.pickTighterStop(side, targetStop, targetExists, atrCandidate)
	}

	roiPct := at.positionRoiPct(side, entryPrice, markPrice, leverage)
	if roiCandidate, ok := at.roiStopCandidate(side, entryPrice, markPrice, leverage, roiPct, gain); ok {
		targetStop, targetExists = at.pickTighterStop(side, targetStop, targetExists, roiCandidate)
	}

	if !targetExists {
		return
	}

	if hasStop {
		if strings.ToLower(side) == "long" && targetStop <= currentStop+floatEpsilon {
			return
		}
		if strings.ToLower(side) == "short" && targetStop >= currentStop-floatEpsilon {
			return
		}
	}

	diff := math.Abs(targetStop - currentStop)
	if hasStop && diff < marketData.CurrentPrice*0.0001 {
		return
	}

	atr := 0.0
	if marketData.MidTermContext != nil {
		atr = marketData.MidTermContext.ATR14
	}
	if err := at.validateStopTightening(symbol, strings.ToLower(side), entryPrice, markPrice, targetStop, atr, leverage); err != nil {
		log.Printf("⚠️ 自动锁盈跳过 (%s %s): %v", symbol, side, err)
		return
	}

	reason := fmt.Sprintf("自动锁盈触发 (ROI %.2f%%)", roiPct)
	at.dispatchAutoStopLoss(symbol, side, targetStop, reason)
}

// evaluateSymbolProtection 在部分平仓/锁盈后重新评估剩余仓位
func (at *AutoTrader) evaluateSymbolProtection(symbol string) {
	positions, err := at.trader.GetPositions()
	if err != nil {
		log.Printf("⚠️ 重新计算锁盈时获取持仓失败: %v", err)
		return
	}
	for _, pos := range positions {
		if posSymbol, _ := pos["symbol"].(string); posSymbol == symbol {
			at.applyDynamicProtection(pos)
		}
	}
}

// 启动回撤监控
func (at *AutoTrader) startDrawdownMonitor() {
	at.monitorWg.Add(1)
	go func() {
		defer at.monitorWg.Done()

		ticker := time.NewTicker(1 * time.Minute) // 每分钟检查一次
		defer ticker.Stop()

		log.Println("📊 启动持仓回撤监控（每分钟检查一次）")

		for {
			select {
			case <-ticker.C:
				at.checkPositionDrawdown()
			case <-at.stopMonitorCh:
				log.Println("⏹ 停止持仓回撤监控")
				return
			}
		}
	}()
}

// 检查持仓回撤情况
func (at *AutoTrader) checkPositionDrawdown() {
	// 获取当前持仓
	positions, err := at.trader.GetPositions()
	if err != nil {
		log.Printf("❌ 回撤监控：获取持仓失败: %v", err)
		return
	}

	for _, pos := range positions {
		symbol := pos["symbol"].(string)
		side := pos["side"].(string)
		entryPrice := pos["entryPrice"].(float64)
		markPrice := pos["markPrice"].(float64)
		quantity := pos["positionAmt"].(float64)
		if quantity < 0 {
			quantity = -quantity // 空仓数量为负，转为正数
		}
		if quantity < floatEpsilon {
			continue
		}

		leverage := 10 // 默认值
		if lev, ok := pos["leverage"].(float64); ok && lev > 0 {
			leverage = int(lev)
		}

		currentPnLPct := at.positionRoiPct(side, entryPrice, markPrice, leverage)
		at.UpdatePeakPnL(symbol, side, currentPnLPct)
		peakPnLPct, exists := at.getPeakPnL(symbol, side)

		if exists && peakPnLPct > 0 && currentPnLPct < peakPnLPct {
			drawdownPct := ((peakPnLPct - currentPnLPct) / peakPnLPct) * 100
			profile := roiProfileFor(leverage)
			if peakPnLPct >= profile.breakeven && drawdownPct >= profile.drawdown {
				log.Printf("🚨 回撤保护触发: %s %s | 当前收益 %.2f%% | 峰值 %.2f%% | 回撤 %.2f%%",
					symbol, side, currentPnLPct, peakPnLPct, drawdownPct)
				if err := at.emergencyClosePosition(symbol, side); err != nil {
					log.Printf("❌ 回撤平仓失败 (%s %s): %v", symbol, side, err)
				} else {
					log.Printf("✅ 回撤平仓成功: %s %s", symbol, side)
					at.ClearPeakPnLCache(symbol, side)
					continue
				}
			}
		}

		// 根据实时盈亏尝试自动锁盈
		at.applyDynamicProtection(pos)
	}
}

// 紧急平仓函数
func (at *AutoTrader) emergencyClosePosition(symbol, side string) error {
	normalizedSide := strings.ToLower(side)
	closedQty := at.getPositionQuantity(symbol, normalizedSide)
	closePrice := 0.0
	if marketData, err := market.Get(symbol); err == nil {
		closePrice = marketData.CurrentPrice
	}

	switch side {
	case "long":
		order, err := at.trader.CloseLong(symbol, 0) // 0 = 全部平仓
		if err != nil {
			return err
		}
		log.Printf("✅ 紧急平多仓成功，订单ID: %v", order["orderId"])
	case "short":
		order, err := at.trader.CloseShort(symbol, 0) // 0 = 全部平仓
		if err != nil {
			return err
		}
		log.Printf("✅ 紧急平空仓成功，订单ID: %v", order["orderId"])
	default:
		return fmt.Errorf("未知的持仓方向: %s", side)
	}

	if closePrice > 0 && closedQty > 0 {
		at.handleRealizedPnL(symbol, normalizedSide, closedQty, closePrice)
	}

	return nil
}

// GetPeakPnLCache 获取最高收益缓存
func (at *AutoTrader) GetPeakPnLCache() map[string]float64 {
	at.peakPnLCacheMutex.RLock()
	defer at.peakPnLCacheMutex.RUnlock()

	// 返回缓存的副本
	cache := make(map[string]float64)
	for k, v := range at.peakPnLCache {
		cache[k] = v
	}
	return cache
}

// UpdatePeakPnL 更新最高收益缓存
func (at *AutoTrader) getPeakPnL(symbol, side string) (float64, bool) {
	key := at.positionKey(symbol, side)
	at.peakPnLCacheMutex.RLock()
	defer at.peakPnLCacheMutex.RUnlock()
	val, ok := at.peakPnLCache[key]
	return val, ok
}

func (at *AutoTrader) UpdatePeakPnL(symbol, side string, currentPnLPct float64) {
	at.peakPnLCacheMutex.Lock()
	defer at.peakPnLCacheMutex.Unlock()

	key := at.positionKey(symbol, side)
	if peak, exists := at.peakPnLCache[key]; exists {
		// 更新峰值（如果是多头，取较大值；如果是空头，currentPnLPct为负，也要比较）
		if currentPnLPct > peak {
			at.peakPnLCache[key] = currentPnLPct
		}
	} else {
		// 首次记录
		at.peakPnLCache[key] = currentPnLPct
	}
}

// ClearPeakPnLCache 清除指定symbol的峰值缓存
func (at *AutoTrader) ClearPeakPnLCache(symbol, side string) {
	at.peakPnLCacheMutex.Lock()
	defer at.peakPnLCacheMutex.Unlock()

	key := at.positionKey(symbol, side)
	delete(at.peakPnLCache, key)
}

func (at *AutoTrader) positionKey(symbol, side string) string {
	return fmt.Sprintf("%s_%s", symbol, strings.ToLower(side))
}

func (at *AutoTrader) estimateStepSize(symbol string) float64 {
	upper := strings.ToUpper(symbol)
	switch {
	case strings.HasPrefix(upper, "BTC"):
		return 0.001
	case strings.HasPrefix(upper, "ETH"):
		return 0.01
	default:
		return 0.1
	}
}

func (at *AutoTrader) roundQuantity(symbol string, qty float64) float64 {
	step := at.estimateStepSize(symbol)
	if step <= 0 {
		step = 0.000001
	}
	steps := math.Floor(qty/step + 1e-9)
	return steps * step
}

func (at *AutoTrader) storePositionMeta(symbol, side string, entryPrice, quantity float64) {
	if quantity <= 0 {
		return
	}
	key := at.positionKey(symbol, side)
	at.positionMetaMutex.Lock()
	defer at.positionMetaMutex.Unlock()
	at.positionMeta[key] = &positionMeta{
		Side:       strings.ToLower(side),
		EntryPrice: entryPrice,
		Quantity:   quantity,
	}
}

func (at *AutoTrader) getPositionQuantity(symbol, side string) float64 {
	key := at.positionKey(symbol, side)
	at.positionMetaMutex.Lock()
	defer at.positionMetaMutex.Unlock()
	if meta, ok := at.positionMeta[key]; ok {
		return meta.Quantity
	}
	return 0
}

func (at *AutoTrader) storeStopLoss(symbol, side string, stopLoss float64) {
	key := at.positionKey(symbol, side)
	at.positionMetaMutex.Lock()
	defer at.positionMetaMutex.Unlock()
	at.stopLossCache[key] = stopLoss
}

func (at *AutoTrader) getStopLoss(symbol, side string) (float64, bool) {
	key := at.positionKey(symbol, side)
	at.positionMetaMutex.Lock()
	defer at.positionMetaMutex.Unlock()
	value, ok := at.stopLossCache[key]
	return value, ok
}

func (at *AutoTrader) storeTakeProfit(symbol, side string, takeProfit float64) {
	key := at.positionKey(symbol, side)
	at.positionMetaMutex.Lock()
	defer at.positionMetaMutex.Unlock()
	at.takeProfitCache[key] = takeProfit
}

func (at *AutoTrader) getTakeProfit(symbol, side string) (float64, bool) {
	key := at.positionKey(symbol, side)
	at.positionMetaMutex.Lock()
	defer at.positionMetaMutex.Unlock()
	value, ok := at.takeProfitCache[key]
	return value, ok
}

func (at *AutoTrader) ensureStopLossTightening(symbol, side string, newStop float64) error {
	key := at.positionKey(symbol, side)
	at.positionMetaMutex.Lock()
	defer at.positionMetaMutex.Unlock()

	// 若无历史记录说明首次设置，直接放行
	previous, exists := at.stopLossCache[key]
	if !exists {
		return nil
	}

	if strings.ToUpper(side) == "LONG" {
		if newStop+floatEpsilon < previous {
			return fmt.Errorf("止损只能收紧，现有止损 %.4f，新止损 %.4f", previous, newStop)
		}
	} else {
		if newStop-floatEpsilon > previous {
			return fmt.Errorf("止损只能收紧，现有止损 %.4f，新止损 %.4f", previous, newStop)
		}
	}
	return nil
}

func (at *AutoTrader) handleRealizedPnL(symbol, side string, closedQuantity, closePrice float64) {
	if closedQuantity <= 0 {
		return
	}

	key := at.positionKey(symbol, side)
	at.positionMetaMutex.Lock()
	meta, exists := at.positionMeta[key]
	if !exists {
		at.positionMetaMutex.Unlock()
		return
	}

	entryPrice := meta.EntryPrice
	remaining := meta.Quantity - closedQuantity
	if remaining < floatEpsilon {
		delete(at.positionMeta, key)
		delete(at.stopLossCache, key)
		delete(at.takeProfitCache, key)
		at.clearStopUpdateTimestamp(symbol, side)
	} else {
		meta.Quantity = remaining
		at.positionMeta[key] = meta
	}
	at.positionMetaMutex.Unlock()

	var pnl float64
	if strings.ToLower(side) == "long" {
		pnl = (closePrice - entryPrice) * closedQuantity
	} else {
		pnl = (entryPrice - closePrice) * closedQuantity
	}

	at.updateConsecutiveLosses(pnl)
}

func (at *AutoTrader) updateConsecutiveLosses(pnl float64) {
	if math.Abs(pnl) < floatEpsilon {
		return
	}

	if pnl < 0 {
		at.consecutiveLosses++
		var pauseDuration time.Duration
		switch at.consecutiveLosses {
		case 2:
			// 连续2笔亏损 → 暂停 45 分钟
			pauseDuration = 45 * time.Minute
		case 3:
			// 连续3笔亏损 → 暂停 24 小时
			pauseDuration = 24 * time.Hour
		default:
			if at.consecutiveLosses >= 4 {
				// 4笔及以上 → 暂停 72 小时，等待人工干预
				pauseDuration = 72 * time.Hour
			}
		}

		if pauseDuration > 0 {
			at.stopUntil = time.Now().Add(pauseDuration)
			log.Printf("🚫 连续亏损 %d 笔，暂停交易 %v，恢复时间 %s", at.consecutiveLosses, pauseDuration, at.stopUntil.Format("2006-01-02 15:04"))
		} else {
			log.Printf("⚠️ 连续亏损 %d 笔，请提高信号质量", at.consecutiveLosses)
		}
	} else {
		if at.consecutiveLosses > 0 {
			// 盈利后立即清零连续亏损计数
			log.Printf("✅ 本笔盈利 %.4f，连续亏损计数重置", pnl)
		}
		at.consecutiveLosses = 0
	}
}
