package eval

import (
	"context"
	"fmt"
	"github.com/go-redis/redis"
	"github.com/zeromicro/go-zero/core/logc"
	"runtime/debug"
	"slices"
	"strings"
	"time"
	"watchAlert/internal/ctx"
	"watchAlert/internal/models"
	"watchAlert/pkg/provider"
	"watchAlert/pkg/tools"

	"golang.org/x/sync/errgroup"
)

type (
	// AlertRuleEval 告警规则评估
	AlertRuleEval interface {
		Submit(rule models.AlertRule)
		Stop(ruleId string)
		Eval(ctx context.Context, rule models.AlertRule)
		Recover(tenantId, ruleId string, eventCacheKey models.AlertEventCacheKey, faultCenterInfoKey models.FaultCenterInfoCacheKey, curFingerprints []string)
		RestartAllEvals()
	}

	// AlertRule 告警规则
	AlertRule struct {
		ctx *ctx.Context
	}
)

func NewAlertRuleEval(ctx *ctx.Context) AlertRuleEval {
	return &AlertRule{
		ctx: ctx,
	}
}

func (t *AlertRule) Submit(rule models.AlertRule) {
	t.ctx.Mux.Lock()
	defer t.ctx.Mux.Unlock()

	c, cancel := context.WithCancel(context.Background())
	t.ctx.ContextMap[rule.RuleId] = cancel
	go t.Eval(c, rule)
}

func (t *AlertRule) Stop(ruleId string) {
	t.ctx.Mux.Lock()
	defer t.ctx.Mux.Unlock()

	if cancel, exists := t.ctx.ContextMap[ruleId]; exists {
		cancel()
		delete(t.ctx.ContextMap, ruleId)
	}
}

func (t *AlertRule) Restart(rule models.AlertRule) {
	t.Stop(rule.RuleId)
	t.Submit(rule)
}

func (t *AlertRule) Eval(ctx context.Context, rule models.AlertRule) {
	taskChan := make(chan struct{}, 1)
	timer := time.NewTicker(t.getEvalTimeDuration(rule.EvalTimeType, rule.EvalInterval))
	defer func() {
		timer.Stop()
		if r := recover(); r != nil {
			// 获取调用栈信息
			stack := debug.Stack()
			logc.Error(t.ctx.Ctx, fmt.Sprintf("Recovered from rule eval goroutine panic: %s, RuleName: %s, RuleId: %s\n%s", r, rule.RuleName, rule.RuleId, stack))
			t.Restart(rule)
		}
	}()

	for {
		select {
		case <-timer.C:
			// 处理任务信号量
			taskChan <- struct{}{}
			t.executeTask(rule, taskChan)
		case <-ctx.Done():
			logc.Infof(t.ctx.Ctx, fmt.Sprintf("停止 RuleId: %v, RuleName: %s 的 Watch 协程", rule.RuleId, rule.RuleName))
			return
		}
		timer.Reset(t.getEvalTimeDuration(rule.EvalTimeType, rule.EvalInterval))
	}
}

func (t *AlertRule) executeTask(rule models.AlertRule, taskChan chan struct{}) {
	defer func() {
		// 释放任务信号量
		<-taskChan
	}()

	// 在规则评估前检查是否仍然启用，避免不必要的操作
	if !t.isRuleEnabled(rule.RuleId) {
		return
	}

	var curFingerprints []string
	for _, dsId := range rule.DatasourceIdList {
		instance, err := t.ctx.DB.Datasource().GetInstance(dsId)
		if err != nil {
			logc.Error(t.ctx.Ctx, err.Error())
			continue
		}

		ok, _ := provider.CheckDatasourceHealth(instance)
		if !ok {
			continue
		}

		if !*instance.Enabled {
			logc.Error(t.ctx.Ctx, fmt.Sprintf("datasource is not enable, id: %s", instance.Id))
			continue
		}

		var fingerprints []string

		switch rule.DatasourceType {
		case "Prometheus", "VictoriaMetrics":
			fingerprints = metrics(t.ctx, dsId, instance.Type, rule)
		case "AliCloudSLS", "Loki", "ElasticSearch", "VictoriaLogs", "ClickHouse":
			fingerprints = logs(t.ctx, dsId, instance.Type, rule)
		case "Jaeger":
			fingerprints = traces(t.ctx, dsId, instance.Type, rule)
		case "CloudWatch":
			fingerprints = cloudWatch(t.ctx, dsId, rule)
		case "KubernetesEvent":
			fingerprints = kubernetesEvent(t.ctx, dsId, rule)
		default:
			continue
		}
		// 追加当前数据源的指纹到总列表
		curFingerprints = append(curFingerprints, fingerprints...)
	}
	//logc.Infof(t.ctx.Ctx, fmt.Sprintf("规则评估 -> %v", tools.JsonMarshal(rule)))
	t.Recover(rule.TenantId, rule.RuleId, models.BuildAlertEventCacheKey(rule.TenantId, rule.FaultCenterId), models.BuildFaultCenterInfoCacheKey(rule.TenantId, rule.FaultCenterId), curFingerprints)
}

// getEvalTimeDuration 获取评估时间
func (t *AlertRule) getEvalTimeDuration(evalTimeType string, evalInterval int64) time.Duration {
	switch evalTimeType {
	case "millisecond":
		return time.Millisecond * time.Duration(evalInterval)
	default:
		return time.Second * time.Duration(evalInterval)
	}
}

func (t *AlertRule) Recover(tenantId, ruleId string, eventCacheKey models.AlertEventCacheKey, faultCenterInfoKey models.FaultCenterInfoCacheKey, curFingerprints []string) {
	// 获取所有的故障中心告警事件
	events, err := t.ctx.Redis.Alert().GetAllEvents(eventCacheKey)
	if err != nil {
		logc.Errorf(t.ctx.Ctx, "AlertRule.Recover: Failed to get all events: %v", err)
		return
	}

	// 存储当前规则下所有活动的指纹
	var activeRuleFingerprints []string

	// 筛选当前规则相关的指纹，并处理预告警状态
	for fingerprint, event := range events {
		if !strings.Contains(event.RuleId, ruleId) {
			continue
		}

		// 移除状态为预告警且当前告警列表中不存在的事件
		if event.Status == models.StatePreAlert && !slices.Contains(curFingerprints, fingerprint) {
			t.ctx.Redis.Alert().RemoveAlertEvent(event.TenantId, event.FaultCenterId, event.Fingerprint)
			continue
		}

		activeRuleFingerprints = append(activeRuleFingerprints, fingerprint)
	}

	/*
		从待恢复状态转换成告警状态（即在 Redis 中存在待恢复 且在 curFingerprints 存在告警的事件）
	*/

	// 获取当前待恢复的告警指纹列表
	pendingFingerprints := t.ctx.Redis.PendingRecover().List(tenantId, ruleId)
	if len(pendingFingerprints) != 0 {
		for _, fingerprint := range curFingerprints {
			if _, exists := pendingFingerprints[fingerprint]; !exists {
				continue
			}
			event, ok := events[fingerprint]
			if !ok {
				continue
			}

			newEvent := event
			// 转换成告警状态
			err := newEvent.TransitionStatus(models.StateAlerting)
			if err != nil {
				logc.Errorf(t.ctx.Ctx, "Failed to transition to「alerting」state for fingerprint %s: %v", fingerprint, err)
				continue
			}
			t.ctx.Redis.Alert().PushAlertEvent(newEvent)
			t.ctx.Redis.PendingRecover().Delete(tenantId, ruleId, fingerprint)
		}
	}

	/*
		从待恢复状态转换成已恢复状态
	*/

	// 计算需要恢复的指纹列表 (即在 Redis 中存在但在当前活动列表中不存在的指纹)
	recoverFingerprints := tools.GetSliceDifference(activeRuleFingerprints, curFingerprints)
	curTime := time.Now().Unix()
	recoverWaitTime := t.getRecoverWaitTime(faultCenterInfoKey)
	for _, fingerprint := range recoverFingerprints {
		event, ok := events[fingerprint]
		if !ok {
			continue
		}

		newEvent := event
		// 获取待恢复状态的时间戳
		wTime, err := t.ctx.Redis.PendingRecover().Get(tenantId, ruleId, fingerprint)
		if err == redis.Nil {
			// 进入待恢复状态, 如果不存在, 则记录当前时间
			t.ctx.Redis.PendingRecover().Set(tenantId, ruleId, fingerprint, curTime)
			// 转换状态, 标记为待恢复
			if err := newEvent.TransitionStatus(models.StatePendingRecovery); err != nil {
				logc.Errorf(t.ctx.Ctx, "Failed to transition to「pending_recovery」state for fingerprint %s: %v", fingerprint, err)
				continue
			}
			t.ctx.Redis.Alert().PushAlertEvent(newEvent)
			continue
		} else if err != nil {
			logc.Errorf(t.ctx.Ctx, "Failed to get「pending_recovery」time for fingerprint %s: %v", fingerprint, err)
			continue
		}

		// 判断是否在等待时间内
		recoverThreshold := wTime + recoverWaitTime
		// 当前时间超过预期等待时间，并且状态是 PendingRecovery 时才执行恢复逻辑
		if curTime >= recoverThreshold && newEvent.Status == models.StatePendingRecovery {
			// 已恢复状态
			if err := newEvent.TransitionStatus(models.StateRecovered); err != nil {
				logc.Errorf(t.ctx.Ctx, "Failed to transition to recovered state for fingerprint %s: %v", fingerprint, err)
				continue
			}
			// 更新告警事件
			t.ctx.Redis.Alert().PushAlertEvent(newEvent)
			// 恢复后继续处理下一个事件
			t.ctx.Redis.PendingRecover().Delete(tenantId, ruleId, fingerprint)
			continue
		}
	}
}

// 获取恢复等待时间
func (t *AlertRule) getRecoverWaitTime(faultCenterInfoKey models.FaultCenterInfoCacheKey) int64 {
	faultCenter := t.ctx.Redis.FaultCenter().GetFaultCenterInfo(faultCenterInfoKey)
	if faultCenter.RecoverWaitTime == 0 {
		return 1
	}
	return faultCenter.RecoverWaitTime
}

func (t *AlertRule) RestartAllEvals() {
	ruleList, err := t.getRuleList()
	if err != nil {
		logc.Error(t.ctx.Ctx, err.Error())
		return
	}

	g := new(errgroup.Group)
	for _, rule := range ruleList {
		rule := rule
		g.Go(func() error {
			t.Submit(rule)
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		logc.Error(t.ctx.Ctx, err.Error())
	}
}

// isRuleEnabled 检查规则是否启用
func (t *AlertRule) isRuleEnabled(ruleId string) bool {
	// 直接检查数据库或缓存中的当前启用状态
	return *t.ctx.DB.Rule().GetRuleObject(ruleId).Enabled
}

func (t *AlertRule) getRuleList() ([]models.AlertRule, error) {
	var ruleList []models.AlertRule
	if err := t.ctx.DB.DB().Where("enabled = ?", "1").Find(&ruleList).Error; err != nil {
		return ruleList, fmt.Errorf("获取 Rule List 失败, err: %s", err.Error())
	}
	return ruleList, nil
}
