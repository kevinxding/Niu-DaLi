package service

import (
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

func formatNotifyType(channelId int, status int) string {
	return fmt.Sprintf("%s_%d_%d", dto.NotifyTypeChannelUpdate, channelId, status)
}

func shouldCloseActiveWebSocketsAfterDisable(channelId int) bool {
	channel, err := model.GetChannelById(channelId, true)
	if err != nil {
		common.SysLog(fmt.Sprintf("failed to check channel status before closing active websockets: channel_id=%d, error=%v", channelId, err))
		return true
	}
	return channel.Status != common.ChannelStatusEnabled
}

// disable & notify
func DisableChannel(channelError types.ChannelError, reason string) {
	common.SysLog(fmt.Sprintf("通道「%s」（#%d）发生错误，准备禁用，原因：%s", channelError.ChannelName, channelError.ChannelId, common.LocalLogPreview(reason)))

	// 检查是否启用自动禁用功能
	if !channelError.AutoBan {
		common.SysLog(fmt.Sprintf("通道「%s」（#%d）未启用自动禁用功能，跳过禁用操作", channelError.ChannelName, channelError.ChannelId))
		return
	}

	success := model.UpdateChannelStatus(channelError.ChannelId, channelError.UsingKey, common.ChannelStatusAutoDisabled, reason)
	if success {
		if shouldCloseActiveWebSocketsAfterDisable(channelError.ChannelId) {
			CloseActiveWebSocketsForChannel(channelError.ChannelId, ChannelDisabledCloseReason)
		}
		subject := fmt.Sprintf("通道「%s」（#%d）已被禁用", channelError.ChannelName, channelError.ChannelId)
		content := fmt.Sprintf("通道「%s」（#%d）已被禁用，原因：%s", channelError.ChannelName, channelError.ChannelId, reason)
		NotifyRootUser(formatNotifyType(channelError.ChannelId, common.ChannelStatusAutoDisabled), subject, content)
	}
}

func EnableChannel(channelId int, usingKey string, channelName string) {
	success := model.UpdateChannelStatus(channelId, usingKey, common.ChannelStatusEnabled, "")
	if success {
		subject := fmt.Sprintf("通道「%s」（#%d）已被启用", channelName, channelId)
		content := fmt.Sprintf("通道「%s」（#%d）已被启用", channelName, channelId)
		NotifyRootUser(formatNotifyType(channelId, common.ChannelStatusEnabled), subject, content)
	}
}

func ShouldDisableChannel(err *types.NewAPIError) bool {
	if !common.AutomaticDisableChannelEnabled {
		return false
	}
	if err == nil {
		return false
	}
	if types.IsChannelError(err) {
		return true
	}
	if types.IsSkipRetryError(err) {
		return false
	}
	if operation_setting.ShouldDisableByStatusCode(err.StatusCode) {
		return true
	}

	lowerMessage := strings.ToLower(err.Error())
	search, _ := AcSearch(lowerMessage, operation_setting.AutomaticDisableKeywords, true)
	return search
}

// balanceExhaustedKeywords identify upstream errors meaning the key ran out of
// balance or prepaid quota, as opposed to an invalid or banned key.
var balanceExhaustedKeywords = []string{
	"insufficient balance",
	"insufficient_balance",
	"insufficient account balance",
	"insufficient_quota",
	"insufficient quota",
	"credit balance is too low",
	"exceeded your current quota",
	"balance is not enough",
	"not enough balance",
	"payment required",
	"余额不足",
	"额度不足",
	"欠费",
}

// IsBalanceExhaustedError reports whether an upstream error means the key has
// run out of balance.
func IsBalanceExhaustedError(err *types.NewAPIError) bool {
	if err == nil {
		return false
	}
	if err.StatusCode == http.StatusPaymentRequired {
		return true
	}
	lowerMessage := strings.ToLower(err.Error())
	found, _ := AcSearch(lowerMessage, balanceExhaustedKeywords, true)
	return found
}

type channelKeyRef struct {
	channelId int
	key       string
}

var (
	keyBalanceFailureLock sync.Mutex
	keyBalanceFailures    = make(map[channelKeyRef]int)
)

// ReachedKeyBalanceFailureThreshold counts one more consecutive
// balance-exhausted failure for a key of a multi-key channel and reports
// whether the key has now failed often enough to be disabled. Reaching the
// threshold clears the count, so a key that is later re-enabled starts over.
func ReachedKeyBalanceFailureThreshold(channelId int, key string) (failures int, reached bool) {
	ref := channelKeyRef{channelId: channelId, key: key}
	keyBalanceFailureLock.Lock()
	defer keyBalanceFailureLock.Unlock()
	failures = keyBalanceFailures[ref] + 1
	if failures >= operation_setting.MultiKeyBalanceFailureThreshold {
		delete(keyBalanceFailures, ref)
		return failures, true
	}
	keyBalanceFailures[ref] = failures
	return failures, false
}

// ResetKeyBalanceFailures clears the consecutive balance failures of a key
// after it served a request successfully.
func ResetKeyBalanceFailures(channelId int, key string) {
	keyBalanceFailureLock.Lock()
	defer keyBalanceFailureLock.Unlock()
	if len(keyBalanceFailures) == 0 {
		return
	}
	delete(keyBalanceFailures, channelKeyRef{channelId: channelId, key: key})
}

// KeyBalanceFailures returns the current consecutive balance failures of a key.
func KeyBalanceFailures(channelId int, key string) int {
	keyBalanceFailureLock.Lock()
	defer keyBalanceFailureLock.Unlock()
	return keyBalanceFailures[channelKeyRef{channelId: channelId, key: key}]
}

func ShouldEnableChannel(newAPIError *types.NewAPIError, status int) bool {
	if !common.AutomaticEnableChannelEnabled {
		return false
	}
	if newAPIError != nil {
		return false
	}
	if status != common.ChannelStatusAutoDisabled {
		return false
	}
	return true
}
