package model

import (
	"fmt"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 守卫: 标了「只有同步接口」的渠道, 对【正常路由】必须不可见。
//
// 为什么这条值得单独一个文件
// --------------------------
// 渠道选择在 new-api 里有【两个实现】(上游 rc.31 起统一成「过滤器」形式, 但仍是两条路):
//
//	MemoryCacheEnabled=true  → channel_cache.go 的 filterCandidateIDs
//	MemoryCacheEnabled=false → ability.go       的 filterAbilitiesByConstraints → ChannelSatisfiesFilters
//
// **生产是 false**(MEMORY_CACHE_ENABLED 没配 = 默认关)。
//
// 2026-08-25 实测事故: 第一版只改了内存那条路, 单测把 MemoryCacheEnabled 设成 true
// 所以【全绿】; 拿真生产库做迁移演练时, 同步腿当场就被正常路由选中并把请求打到了上游
// (日志原文 `channel error (channel #99, status code: 400)`)。
// ⇒ 判据只在你【不关心】的那种情况下成立, 等于没有判据。
//
// 上游 rc.31 把 DB 那条路改成「先把候选全查出来、在 Go 里过滤、过滤之后再算优先级档位」,
// 所以 08-25 第二个事故(同步腿优先级最高 → 独占最高档 → 过滤后那档空了 → 全站无渠道)
// 在结构上已经不可能了。下面仍然保留那条测试, 并且改成端到端走 GetChannel —— 这回
// sqlite 上也能跑通了(上游不再用那条在 sqlite 上报语法错的子查询)。

func setupVisibilityTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	origDB, origCache, origGroupCol := DB, common.MemoryCacheEnabled, commonGroupCol
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&Channel{}, &Ability{}))
	DB = db
	commonGroupCol = "`group`"
	common.MemoryCacheEnabled = false
	t.Cleanup(func() {
		DB, common.MemoryCacheEnabled, commonGroupCol = origDB, origCache, origGroupCol
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

// seedVisibilityChannel 建一个渠道 + 它在 default/gpt-image-2 上的路由行。
func seedVisibilityChannel(t *testing.T, db *gorm.DB, id int, priority int64, syncRelay bool) {
	t.Helper()
	p, w := priority, uint(100)
	ch := &Channel{
		Id: id, Type: 1, Key: fmt.Sprintf("k%d", id), Status: common.ChannelStatusEnabled,
		Name: fmt.Sprintf("ch%d", id), Weight: &w, Models: "gpt-image-2",
		Group: "default", Priority: &p,
	}
	if syncRelay {
		ch.SetSetting(kitdto.ChannelSettings{QuriovSyncImageRelay: true})
	}
	require.NoError(t, db.Create(ch).Error)
	require.NoError(t, db.Create(&Ability{
		Group: "default", Model: "gpt-image-2", ChannelId: id,
		Enabled: true, Priority: &p, Weight: 100,
	}).Error)
}

func pathFilters(path string) []dto.ChannelFilter {
	if path == "" {
		return nil
	}
	return []dto.ChannelFilter{{Kind: dto.FilterRequestPath, RequestPath: path}}
}

// ⭐ DB 那条路（生产走的就是这条）。带不带过滤器都得排除 —— 上游在没有过滤器时
// 不会进过滤循环, 只挂在过滤器里的排除会在「没过滤器」那一支上漏掉。
func TestFilterAbilities_ExcludesSyncRelayChannels(t *testing.T) {
	db := setupVisibilityTestDB(t)
	seedVisibilityChannel(t, db, 2, 0, false) // 正常异步渠道
	seedVisibilityChannel(t, db, 9, 0, true)  // 同步腿

	abilities := []Ability{{ChannelId: 2}, {ChannelId: 9}}
	for _, path := range []string{"/v1/videos", ""} {
		got := filterAbilitiesByConstraints(abilities, "gpt-image-2", pathFilters(path))
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
	seedVisibilityChannel(t, db, 2, 0, false)
	seedVisibilityChannel(t, db, 3, 0, false)

	got := filterAbilitiesByConstraints([]Ability{{ChannelId: 2}, {ChannelId: 3}}, "gpt-image-2", pathFilters("/v1/videos"))
	assert.Len(t, got, 2, "普通渠道被误杀了 —— 那会让正常请求无渠道可用")
}

// ⭐ 内存缓存那条路
func TestFilterChannels_ExcludesSyncRelayChannels(t *testing.T) {
	db := setupVisibilityTestDB(t)
	common.MemoryCacheEnabled = true
	seedVisibilityChannel(t, db, 2, 0, false)
	seedVisibilityChannel(t, db, 9, 0, true)
	primeChannelCache(t, 2, 9)

	for _, path := range []string{"/v1/videos", ""} {
		got, _ := filterCandidateIDs([]int{2, 9}, "gpt-image-2", pathFilters(path))
		assert.Equal(t, []int{2}, got,
			"requestPath=%q 时同步腿仍然进了正常路由（内存缓存路径）", path)
	}
}

func TestFilterChannels_KeepsNormalChannels(t *testing.T) {
	db := setupVisibilityTestDB(t)
	common.MemoryCacheEnabled = true
	seedVisibilityChannel(t, db, 2, 0, false)
	seedVisibilityChannel(t, db, 3, 0, false)
	primeChannelCache(t, 2, 3)

	got, _ := filterCandidateIDs([]int{2, 3}, "gpt-image-2", pathFilters("/v1/videos"))
	assert.Equal(t, []int{2, 3}, got)
}

// ⭐⭐ 钉住「两条路都得有」这件事本身。
//
// 最容易发生的退化是: 有人改其中一个实现时忘了另一个, 而**生产只走其中一条**。
// 这条测试拿同一组输入喂给两个过滤器, 要求它们给出一致的结论。
func TestBothChannelSelectionPathsAgreeOnSyncRelay(t *testing.T) {
	db := setupVisibilityTestDB(t)
	common.MemoryCacheEnabled = true
	seedVisibilityChannel(t, db, 2, 0, false)
	seedVisibilityChannel(t, db, 9, 0, true)
	primeChannelCache(t, 2, 9)

	viaCache, _ := filterCandidateIDs([]int{2, 9}, "gpt-image-2", pathFilters("/v1/videos"))

	viaDB := make([]int, 0, 2)
	for _, a := range filterAbilitiesByConstraints([]Ability{{ChannelId: 2}, {ChannelId: 9}}, "gpt-image-2", pathFilters("/v1/videos")) {
		viaDB = append(viaDB, a.ChannelId)
	}

	assert.Equal(t, viaCache, viaDB,
		"两条渠道选择路径的结论不一致 —— 生产走的是【DB 那条】(MEMORY_CACHE_ENABLED 默认关)，"+
			"只改内存那条等于没改。2026-08-25 就是这么放过一个真 bug 的。")
	assert.Equal(t, []int{2}, viaCache, "两条都该只剩正常渠道")
}

// ⭐⭐⭐ 「过滤放错位置」比"没过滤"更危险 —— 后果是【全站故障】。
//
// 2026-08-25 在真库上实测撞到: 同步腿 priority=999、正常渠道 priority=10 →
// 同步腿独占最高档 → 过滤后那一档空了 → "无可用渠道" → 正常客户请求全废。
// 这里端到端走生产那条 DB 路径(GetRandomSatisfiedChannel → GetChannel), 同步腿优先级故意设得最高。
func TestSyncRelayWithHigherPriorityDoesNotBlockNormalChannels(t *testing.T) {
	db := setupVisibilityTestDB(t)
	seedVisibilityChannel(t, db, 2, 10, false)
	seedVisibilityChannel(t, db, 9, 999, true)

	for i := 0; i < 20; i++ { // 随机选渠道, 多抽几次
		ch, err := GetRandomSatisfiedChannel("default", "gpt-image-2", 0, pathFilters("/v1/videos"))
		require.NoError(t, err)
		require.NotNil(t, ch,
			"最高档被同步腿独占并清空了 —— 正常渠道轮不到, 这是【全站故障】不是隔离生效")
		assert.Equal(t, 2, ch.Id, "同步腿不该出现在正常路由的候选里")
	}
}

// 同步腿不许占掉一个「优先级档位」——占了会白白吃掉一次重试机会。
//
// ⚠ 这条要【三个】渠道才测得出来:
//
//	排除了:  档位=[100,10]     retry=1 → 10   ← 第二次重试就用上最后一条腿
//	没排除:  档位=[999,100,10] retry=1 → 100  ← 一次重试白白花在空档上
func TestSyncRelayDoesNotConsumeAPriorityTier(t *testing.T) {
	db := setupVisibilityTestDB(t)
	seedVisibilityChannel(t, db, 9, 999, true) // 同步腿，优先级最高
	seedVisibilityChannel(t, db, 2, 100, false)
	seedVisibilityChannel(t, db, 3, 10, false)

	ch, err := GetRandomSatisfiedChannel("default", "gpt-image-2", 1, pathFilters("/v1/videos"))
	require.NoError(t, err)
	require.NotNil(t, ch)
	assert.Equal(t, 3, ch.Id,
		"第一次重试应该直接落到最后一条正常腿(优先级 10)。拿到 #2 说明同步腿占了一档, "+
			"一次重试机会被白白花在一个会被过滤空的档位上")
}

// 重投那条路要能把同步腿【单独】挑出来 —— 它对正常路由不可见, 但不能对重投也不可见。
func TestGetSyncRelayChannels_DBPathFindsOnlySyncLegs(t *testing.T) {
	db := setupVisibilityTestDB(t)
	seedVisibilityChannel(t, db, 2, 10, false)
	seedVisibilityChannel(t, db, 9, 0, true)

	got := GetSyncRelayChannels("default", "gpt-image-2")
	require.Len(t, got, 1, "同步腿没被挑出来 —— 重投将无腿可换（而且是静默的）")
	assert.Equal(t, 9, got[0].Id)
}
