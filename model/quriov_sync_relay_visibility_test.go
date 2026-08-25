package model

import (
	"fmt"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 守卫: 标了「只有同步接口」的渠道, 对【正常路由】必须不可见。
//
// 为什么这条值得单独一个文件
// --------------------------
// 渠道选择在 new-api 里有【两个实现】:
//
//	MemoryCacheEnabled=true  → channel_cache.go 的 filterChannelsByRequestPathAndModel
//	MemoryCacheEnabled=false → ability.go       的 filterAbilitiesByRequestPathAndModel
//
// **生产是 false**(MEMORY_CACHE_ENABLED 没配 = 默认关)。
//
// 2026-08-25 实测事故: 第一版只改了内存那条路, 单测把 MemoryCacheEnabled 设成 true
// 所以【全绿】; 拿真生产库做迁移演练时, 同步腿当场就被正常路由选中并把请求打到了上游
// (日志原文 `channel error (channel #99, status code: 400)`)。
// ⇒ 判据只在你【不关心】的那种情况下成立, 等于没有判据。
//
// ⚠ 本文件直接测两个过滤函数, 不走 GetRandomSatisfiedChannel ——
//   因为 DB 那条路的查询在 sqlite 上本来就报语法错误(上游自带, 与本改动无关),
//   端到端只能拿真 MySQL 验(见 QURIOV.md 的迁移演练一节)。
//   把这句写下来是为了**不让人误以为 DB 那条路已经端到端覆盖了**。

func setupVisibilityTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	origDB, origCache := DB, common.MemoryCacheEnabled
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Channel{}, &Ability{}))
	DB = db
	t.Cleanup(func() {
		DB, common.MemoryCacheEnabled = origDB, origCache
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// primeChannelCache 直接把渠道喂进内存缓存。
// 不用 InitChannelCache: 它还会去初始化定价等一堆跟本测试无关的全局状态,
// 在这个精简环境里会 panic。我们要测的只是那个过滤函数。
func primeChannelCache(t *testing.T, ids ...int) {
	t.Helper()
	orig := channelsIDM
	m := make(map[int]*Channel, len(ids))
	for _, id := range ids {
		var ch Channel
		require.NoError(t, DB.First(&ch, id).Error)
		m[id] = &ch
	}
	channelsIDM = m
	t.Cleanup(func() { channelsIDM = orig })
}

func seedVisibilityChannel(t *testing.T, db *gorm.DB, id int, syncRelay bool) {
	t.Helper()
	p, w := int64(0), uint(100)
	ch := &Channel{
		Id: id, Type: 1, Key: fmt.Sprintf("k%d", id), Status: common.ChannelStatusEnabled,
		Name: fmt.Sprintf("ch%d", id), Weight: &w, Models: "gpt-image-2",
		Group: "default", Priority: &p,
	}
	if syncRelay {
		ch.SetSetting(dto.ChannelSettings{QuriovSyncImageRelay: true})
	}
	require.NoError(t, db.Create(ch).Error)
}

// ⭐ DB 那条路（生产走的就是这条）
func TestFilterAbilities_ExcludesSyncRelayChannels(t *testing.T) {
	db := setupVisibilityTestDB(t)
	seedVisibilityChannel(t, db, 2, false) // 正常异步渠道
	seedVisibilityChannel(t, db, 9, true)  // 同步腿

	abilities := []Ability{{ChannelId: 2}, {ChannelId: 9}}
	for _, path := range []string{"/v1/videos", ""} {
		got := filterAbilitiesByRequestPathAndModel(abilities, path, "gpt-image-2")
		ids := make([]int, 0, len(got))
		for _, a := range got {
			ids = append(ids, a.ChannelId)
		}
		assert.Equal(t, []int{2}, ids,
			"requestPath=%q 时同步腿仍然进了正常路由 —— 客户的首次请求会被发给一个接不了异步请求的上游", path)
	}
}

// 反向: 全是正常渠道时一个都不许被误杀。
// 没有这条的话，「把所有渠道都过滤掉」也能让上面那条通过。
func TestFilterAbilities_KeepsNormalChannels(t *testing.T) {
	db := setupVisibilityTestDB(t)
	seedVisibilityChannel(t, db, 2, false)
	seedVisibilityChannel(t, db, 3, false)

	got := filterAbilitiesByRequestPathAndModel([]Ability{{ChannelId: 2}, {ChannelId: 3}}, "/v1/videos", "gpt-image-2")
	assert.Len(t, got, 2, "普通渠道被误杀了 —— 那会让正常请求无渠道可用")
}

// ⭐ 内存缓存那条路
func TestFilterChannels_ExcludesSyncRelayChannels(t *testing.T) {
	db := setupVisibilityTestDB(t)
	common.MemoryCacheEnabled = true
	seedVisibilityChannel(t, db, 2, false)
	seedVisibilityChannel(t, db, 9, true)
	primeChannelCache(t, 2, 9)

	for _, path := range []string{"/v1/videos", ""} {
		got := filterChannelsByRequestPathAndModel([]int{2, 9}, path, "gpt-image-2")
		assert.Equal(t, []int{2}, got,
			"requestPath=%q 时同步腿仍然进了正常路由（内存缓存路径）", path)
	}
}

func TestFilterChannels_KeepsNormalChannels(t *testing.T) {
	db := setupVisibilityTestDB(t)
	common.MemoryCacheEnabled = true
	seedVisibilityChannel(t, db, 2, false)
	seedVisibilityChannel(t, db, 3, false)
	primeChannelCache(t, 2, 3)

	assert.Equal(t, []int{2, 3}, filterChannelsByRequestPathAndModel([]int{2, 3}, "/v1/videos", "gpt-image-2"))
}

// ⭐⭐ 钉住「两条路都得有」这件事本身。
//
// 最容易发生的退化是: 有人改其中一个实现时忘了另一个, 而**生产只走其中一条**。
// 这条测试拿同一组输入喂给两个过滤器, 要求它们给出一致的结论。
func TestBothChannelSelectionPathsAgreeOnSyncRelay(t *testing.T) {
	db := setupVisibilityTestDB(t)
	common.MemoryCacheEnabled = true
	seedVisibilityChannel(t, db, 2, false)
	seedVisibilityChannel(t, db, 9, true)
	primeChannelCache(t, 2, 9)

	viaCache := filterChannelsByRequestPathAndModel([]int{2, 9}, "/v1/videos", "gpt-image-2")

	viaDB := make([]int, 0, 2)
	for _, a := range filterAbilitiesByRequestPathAndModel([]Ability{{ChannelId: 2}, {ChannelId: 9}}, "/v1/videos", "gpt-image-2") {
		viaDB = append(viaDB, a.ChannelId)
	}

	assert.Equal(t, viaCache, viaDB,
		"两条渠道选择路径的结论不一致 —— 生产走的是【DB 那条】(MEMORY_CACHE_ENABLED 默认关)，"+
			"只改内存那条等于没改。2026-08-25 就是这么放过一个真 bug 的。")
	assert.Equal(t, []int{2}, viaCache, "两条都该只剩正常渠道")
}
