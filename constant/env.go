package constant

var StreamingTimeout int
var DifyDebug bool
var MaxFileDownloadMB int
var StreamScannerMaxBufferMB int
var ForceStreamOption bool
var CountToken bool
var GetMediaToken bool
var GetMediaTokenNotStream bool
var UpdateTask bool
var MaxRequestBodyMB int
var AnonymousRequestBodyLimitKB int
var AzureDefaultAPIVersion string
var NotifyLimitCount int
var NotificationLimitDurationMinute int
var GenerateDefaultToken bool
var ErrorLogEnabled bool
var TaskQueryLimit int
var TaskTimeoutMinutes int

// ── 任务失败后换渠道重投（Quriov 改造）────────────────────────────────
// 上游把任务【收下了】之后才失败的那一类，new-api 原本完全没有兜底：
// 提交阶段的渠道重试在任务落库那一刻就结束了，之后无论上游怎么失败都不会换渠道。
// 实测(2026-08-21)：某天 28 次任务失败全部落在同一个渠道，配好的备用渠道一次都没被试过。
var TaskResubmitEnabled bool    // 总开关
var TaskResubmitMaxAttempts int // 单个任务最多额外重投几次（不含首次提交）
var TaskResubmitMaxBodyKB int   // 超过这个大小的请求体不留存，也就无法重投
// TaskResubmitSkipReasons 命中即判定为「换家上游也没用」，直接终态失败不重投。
// 典型是内容审核/提示词被拒——换个渠道一样拒，重投只是把一次收费变两次。
var TaskResubmitSkipReasons []string

// temporary variable for sora patch, will be removed in future
var TaskPricePatches []string

// TrustedRedirectDomains is a list of trusted domains for redirect URL validation.
// Domains support subdomain matching (e.g., "example.com" matches "sub.example.com").
var TrustedRedirectDomains []string
