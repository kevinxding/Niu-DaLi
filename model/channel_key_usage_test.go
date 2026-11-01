package model

import (
	"os"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
)

func TestChannelKeyUsageDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			// In-memory SQLite gives every connection its own database.
			maxOpenConns := 8
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(":memory:")
				maxOpenConns = 1
			case "mysql":
				dsn := os.Getenv("TEST_MYSQL_DSN")
				if dsn == "" {
					t.Skip("TEST_MYSQL_DSN not configured")
				}
				driver = mysql.Open(dsn)
			case "postgres":
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("TEST_POSTGRES_DSN not configured")
				}
				driver = postgres.Open(dsn)
			}
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "key_usage_test_"}})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(maxOpenConns)
			previousDB, previousType, previousBatch := DB, common.MainDatabaseType(), common.BatchUpdateEnabled
			DB = db
			common.SetMainDatabaseType(common.DatabaseType(dialect))
			common.BatchUpdateEnabled = false
			t.Cleanup(func() {
				require.NoError(t, db.Migrator().DropTable(&ChannelKeyUsage{}))
				DB = previousDB
				common.SetMainDatabaseType(previousType)
				common.BatchUpdateEnabled = previousBatch
				require.NoError(t, sqlDB.Close())
			})
			// Startup migration runs on every boot and must be idempotent.
			require.NoError(t, db.AutoMigrate(&ChannelKeyUsage{}))
			require.NoError(t, db.AutoMigrate(&ChannelKeyUsage{}))

			keyA, keyB, keyC := ChannelKeyHash("key-a"), ChannelKeyHash("key-b"), ChannelKeyHash("key-c")
			UpdateChannelKeyUsedQuota(1, keyA, 100)
			UpdateChannelKeyUsedQuota(1, keyA, 50)
			UpdateChannelKeyUsedQuota(1, keyB, 30)
			UpdateChannelKeyUsedQuota(1, keyA, -20) // task refund
			UpdateChannelKeyUsedQuota(2, keyA, 7)   // same key on another channel
			UpdateChannelKeyUsedQuota(1, keyB, 0)
			UpdateChannelKeyUsedQuota(1, "", 99)

			// Concurrent first charges of a new key race on the unique index;
			// every one of them must still be counted.
			var wg sync.WaitGroup
			for range 20 {
				wg.Go(func() { UpdateChannelKeyUsedQuota(1, keyC, 5) })
			}
			wg.Wait()

			quotas, err := GetChannelKeyUsedQuotas(1)
			require.NoError(t, err)
			assert.Equal(t, map[string]int64{keyA: 130, keyB: 30, keyC: 100}, quotas)
			quotas, err = GetChannelKeyUsedQuotas(2)
			require.NoError(t, err)
			assert.Equal(t, map[string]int64{keyA: 7}, quotas)

			assert.Error(t, db.Create(&ChannelKeyUsage{ChannelId: 1, KeyHash: keyA}).Error, "unique index must reject a second row for the same key")
		})
	}
}

func TestUpdateChannelKeyUsedQuotaBatchesUntilFlush(t *testing.T) {
	require.NoError(t, DB.Exec("DELETE FROM channel_key_usages").Error)
	batchUpdateEnabled := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = true
	t.Cleanup(func() {
		common.BatchUpdateEnabled = batchUpdateEnabled
		DB.Exec("DELETE FROM channel_key_usages")
	})

	keyA := ChannelKeyHash("key-a")
	UpdateChannelKeyUsedQuota(3, keyA, 40)
	UpdateChannelKeyUsedQuota(3, keyA, 60)

	quotas, err := GetChannelKeyUsedQuotas(3)
	require.NoError(t, err)
	assert.Empty(t, quotas)

	batchUpdate()

	quotas, err = GetChannelKeyUsedQuotas(3)
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{keyA: 100}, quotas)
}
