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

// ⭐⭐⭐ 这条钉的是「过滤放错位置」——它比"没过滤"更危险，因为后果是【全站故障】。
//
// 2026-08-25 在真库上实测撞到:
//
//	同步腿 priority=999、正常渠道 priority=10 →
//	优先级档位在 SQL 里用 MAX(priority) 算, 同步腿独占最高档 →
//	之后再把它过滤掉, 那一档空了 → "无可用渠道" →
//	**优先级更低的正常渠道根本轮不到**。正常客户请求直接全废。
//
// 所以排除必须发生在【算档位之前】(SQL 层), 不能查出来再过滤。
// 这条测试专门盯这个位置: 同步腿优先级设得比正常渠道【高】。
func TestSyncRelayWithHigherPriorityDoesNotBlockNormalChannels(t *testing.T) {
	db := setupVisibilityTestDB(t)

	p10, p999, w := int64(10), int64(999), uint(100)
	require.NoError(t, db.Create(&Channel{
		Id: 2, Type: 1, Key: "k2", Status: common.ChannelStatusEnabled, Name: "normal",
		Weight: &w, Models: "gpt-image-2", Group: "default", Priority: &p10,
	}).Error)
	sync := &Channel{
		Id: 9, Type: 1, Key: "k9", Status: common.ChannelStatusEnabled, Name: "sync",
		Weight: &w, Models: "gpt-image-2", Group: "default", Priority: &p999,
	}
	sync.SetSetting(dto.ChannelSettings{QuriovSyncImageRelay: true})
	require.NoError(t, db.Create(sync).Error)
	require.NoError(t, db.Create(&Ability{Group: "default", Model: "gpt-image-2", ChannelId: 2, Enabled: true, Priority: &p10, Weight: 100}).Error)
	require.NoError(t, db.Create(&Ability{Group: "default", Model: "gpt-image-2", ChannelId: 9, Enabled: true, Priority: &p999, Weight: 100}).Error)

	// 档位表里不许出现同步腿那一档 —— 出现了就会浪费一次重试在空档上。
	ids := syncRelayChannelIDs()
	assert.Equal(t, []int{9}, ids, "同步腿必须被识别出来")

	q, err := getChannelQuery("default", "gpt-image-2", 0)
	require.NoError(t, err)
	var abilities []Ability
	require.NoError(t, q.Find(&abilities).Error)
	require.NotEmpty(t, abilities,
		"最高档被同步腿独占并清空了 —— 正常渠道轮不到, 这是【全站故障】不是隔离生效")
	for _, a := range abilities {
		assert.NotEqual(t, 9, a.ChannelId, "同步腿不该出现在正常路由的候选里")
	}
	assert.Equal(t, 2, abilities[0].ChannelId, "第一档应该直接就是那个正常渠道")
}

// 同步腿不许占掉一个「优先级档位」——占了会白白吃掉一次重试机会。
//
// ⚠ 这条要【三个】渠道才测得出来。两个的时候两种实现给的答案一样:
//
//	  排除了:  档位=[10]        retry=1 → 超出范围, 取最小 = 10
//	  没排除:  档位=[999,10]    retry=1 → 10
//	一样。所以第一版(只有两个渠道)让「getPriority 不排除」这个变异活了下来。
//	三个渠道才分得开:
//	  排除了:  档位=[100,10]     retry=1 → 10   ← 第二次重试就用上最后一条腿
//	  没排除:  档位=[999,100,10] retry=1 → 100  ← 一次重试白白花在空档上
func TestSyncRelayDoesNotConsumeAPriorityTier(t *testing.T) {
	db := setupVisibilityTestDB(t)

	mk := func(id int, prio int64, sync bool) {
		w := uint(100)
		p := prio
		ch := &Channel{
			Id: id, Type: 1, Key: fmt.Sprintf("k%d", id), Status: common.ChannelStatusEnabled,
			Name: fmt.Sprintf("ch%d", id), Weight: &w, Models: "gpt-image-2",
			Group: "default", Priority: &p,
		}
		if sync {
			ch.SetSetting(dto.ChannelSettings{QuriovSyncImageRelay: true})
		}
		require.NoError(t, db.Create(ch).Error)
		require.NoError(t, db.Create(&Ability{
			Group: "default", Model: "gpt-image-2", ChannelId: id,
			Enabled: true, Priority: &p, Weight: 100,
		}).Error)
	}
	mk(9, 999, true)  // 同步腿，优先级最高
	mk(2, 100, false) // 正常渠道 A
	mk(3, 10, false)  // 正常渠道 B

	got, err := getPriority("default", "gpt-image-2", 1)
	require.NoError(t, err)
	assert.Equal(t, 10, got,
		"第一次重试应该直接落到最后一条正常腿(优先级 10)。拿到 100 说明同步腿占了一档, "+
			"一次重试机会被白白花在一个会被过滤空的档位上")
}
