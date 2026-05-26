package locker

import (
	"context"
	"time"
)

// Metrics 指标名称
const (
	MetricAcquire     = "lock.acquire"
	MetricAcquireFail = "lock.acquire.fail"
	MetricAcquireDur  = "lock.acquire.duration"
	MetricRelease     = "lock.release"
	MetricRenew       = "lock.renew"
	MetricRenewFail   = "lock.renew.fail"
	MetricRenewDur    = "lock.renew.duration"
	MetricLost             = "lock.lost"
	MetricFencingTokenFail = "lock.acquire.fencing_fail"
)

// Span 名称
const (
	SpanLock    = "lock.Lock"
	SpanTryLock = "lock.TryLock"
	SpanUnLock  = "lock.UnLock"
	SpanRenew   = "lock.renew"
	SpanLost    = "lock.lost"
)

// Attr key
const (
	AttrKeyStr    = "lock.key"
	AttrOwnerStr  = "lock.owner"
	AttrReasonStr = "lock.reason"
)

// Attr 键值对属性，用于 Metrics 和 Tracer 的标签/属性
type Attr struct {
	Key   string
	Value string
}

// AttrKey 构造锁键名属性
func AttrKey(v string) Attr { return Attr{Key: AttrKeyStr, Value: v} }

// AttrOwner 构造锁持有者属性
func AttrOwner(v string) Attr { return Attr{Key: AttrOwnerStr, Value: v} }

// AttrReason 构造原因属性
func AttrReason(v string) Attr { return Attr{Key: AttrReasonStr, Value: v} }

// Metrics 分布式锁指标采集接口
//
// 指标名称约定（对应 Metric* 常量）：
//   - MetricAcquire          加锁成功
//   - MetricAcquireFail      加锁失败
//   - MetricFencingTokenFail Fencing Token 生成失败（加锁成功，但 fencing 降级）
//   - MetricRelease          解锁
//   - MetricRenew            续期成功
//   - MetricRenewFail        续期失败
//   - MetricLost             锁丢失
type Metrics interface {
	// IncrementCounter 递增计数器指标
	IncrementCounter(name string, attrs ...Attr)

	// RecordDuration 记录耗时指标
	RecordDuration(name string, d time.Duration, attrs ...Attr)
}

// Tracer 分布式锁链路追踪接口
type Tracer interface {
	// StartSpan 创建一个 Span，调用方负责调用 End()
	//
	// Span 名称约定（对应 Span* 常量）：
	//   - SpanLock    Lock 操作
	//   - SpanTryLock TryLock 操作
	//   - SpanUnLock  UnLock 操作
	//   - SpanRenew   看门狗续期
	StartSpan(ctx context.Context, name string, attrs ...Attr) (context.Context, Span)
}

// Span 链路追踪 Span
type Span interface {
	End()
	SetAttributes(...Attr)
	RecordError(error)
}
