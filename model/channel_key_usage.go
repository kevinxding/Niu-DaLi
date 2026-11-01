package model

import (
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/QuantumNous/new-api/common"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// ChannelKeyUsage accumulates the quota consumed through one key of a
// multi-key channel. Keys are identified by a SHA-256 hash rather than by
// position, so deleting or reordering keys never moves usage between keys and
// the table never stores a usable secret.
type ChannelKeyUsage struct {
	Id        int    `json:"id"`
	ChannelId int    `json:"channel_id" gorm:"not null;uniqueIndex:idx_channel_key_usages_channel_key,priority:1"`
	KeyHash   string `json:"key_hash" gorm:"type:varchar(64);not null;uniqueIndex:idx_channel_key_usages_channel_key,priority:2"`
	UsedQuota int64  `json:"used_quota" gorm:"bigint;not null;default:0"`
}

type channelKeyUsageRef struct {
	channelId int
	keyHash   string
}

var (
	channelKeyUsageBatchLock  sync.Mutex
	channelKeyUsageBatchStore = make(map[channelKeyUsageRef]int64)
)

func ChannelKeyHash(key string) string {
	return hex.EncodeToString(common.Sha256Raw([]byte(key)))
}

// UpdateChannelKeyUsedQuota adds quota (negative for refunds) to the usage of
// the key identified by keyHash. It mirrors UpdateChannelUsedQuota, including
// batch accumulation when batch updates are enabled.
func UpdateChannelKeyUsedQuota(channelId int, keyHash string, quota int) {
	if keyHash == "" || quota == 0 {
		return
	}
	if common.BatchUpdateEnabled {
		channelKeyUsageBatchLock.Lock()
		channelKeyUsageBatchStore[channelKeyUsageRef{channelId: channelId, keyHash: keyHash}] += int64(quota)
		channelKeyUsageBatchLock.Unlock()
		return
	}
	updateChannelKeyUsedQuota(channelId, keyHash, int64(quota))
}

func updateChannelKeyUsedQuota(channelId int, keyHash string, quota int64) {
	increment := func() *gorm.DB {
		return DB.Model(&ChannelKeyUsage{}).
			Where("channel_id = ? AND key_hash = ?", channelId, keyHash).
			Update("used_quota", gorm.Expr("used_quota + ?", quota))
	}
	result := increment()
	if result.Error == nil && result.RowsAffected == 0 {
		// First charge through this key. A concurrent first charge can win the
		// unique index, in which case the row now exists and incrementing works,
		// so the expected duplicate-key error is not logged.
		quiet := DB.Session(&gorm.Session{Logger: DB.Logger.LogMode(gormlogger.Silent)})
		if err := quiet.Create(&ChannelKeyUsage{ChannelId: channelId, KeyHash: keyHash, UsedQuota: quota}).Error; err != nil {
			result = increment()
		}
	}
	if result.Error != nil {
		common.SysLog(fmt.Sprintf("failed to update channel key used quota: channel_id=%d, delta_quota=%d, error=%v", channelId, quota, result.Error))
	}
}

func flushChannelKeyUsageBatch() {
	channelKeyUsageBatchLock.Lock()
	store := channelKeyUsageBatchStore
	channelKeyUsageBatchStore = make(map[channelKeyUsageRef]int64)
	channelKeyUsageBatchLock.Unlock()
	for ref, quota := range store {
		if quota != 0 {
			updateChannelKeyUsedQuota(ref.channelId, ref.keyHash, quota)
		}
	}
}

// GetChannelKeyUsedQuotas returns the used quota of each key of a channel,
// keyed by ChannelKeyHash.
func GetChannelKeyUsedQuotas(channelId int) (map[string]int64, error) {
	var usages []ChannelKeyUsage
	if err := DB.Where("channel_id = ?", channelId).Find(&usages).Error; err != nil {
		return nil, err
	}
	quotas := make(map[string]int64, len(usages))
	for _, usage := range usages {
		quotas[usage.KeyHash] = usage.UsedQuota
	}
	return quotas, nil
}
