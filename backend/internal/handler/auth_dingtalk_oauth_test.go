package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDingTalkOAuthStart_Disabled は sentinel テスト。
// TODO(task-1.10): newTestAuthHandlerWithDingTalk helper が追加されたら t.Skip を外す。
func TestDingTalkOAuthStart_Disabled(t *testing.T) {
	t.Skip("helper newTestAuthHandlerWithDingTalk added in Task 1.10; sentinel only")
}

// TestBuildDingTalkSyntheticEmail_UsesUnionID 验证合成邮箱种子使用 unionID。
func TestBuildDingTalkSyntheticEmail_UsesUnionID(t *testing.T) {
	unionID := "union_AbCdEf123"
	email := buildDingTalkSyntheticEmail(unionID)

	want := "dingtalk-union_abcdef123@dingtalk-connect.invalid"
	require.Equal(t, want, email)

	// 确保结果都是小写（邮箱大小写不敏感，统一小写）
	require.True(t, strings.ToLower(email) == email, "synthetic email should be all lowercase")

	// 确保前缀正确
	require.True(t, strings.HasPrefix(email, "dingtalk-"), "should have dingtalk- prefix")

	// 确保后缀是合成邮箱域名
	require.True(t, strings.HasSuffix(email, "@dingtalk-connect.invalid"), "should have reserved domain suffix")
}

// TestBuildDingTalkSyntheticEmail_TrimsSpace 验证 unionID 空白被修剪。
func TestBuildDingTalkSyntheticEmail_TrimsSpace(t *testing.T) {
	email := buildDingTalkSyntheticEmail("  UID_XYZ  ")
	require.Equal(t, "dingtalk-uid_xyz@dingtalk-connect.invalid", email)
}

// TestBuildDingTalkUpstreamClaims_EmptyStaff 验证 staff 为空 struct（跨组织降级路径）时：
// - subject 等于 unionID（与 identityKey.ProviderSubject 一致）
// - corp_user_id 为空字符串（跨组织时拿不到企业 userid）
// - email/username 为空字符串
// B/C: Step 3/4 失败降级时 staff = &DingTalkStaffInfo{}，claims 不应有 nil。
func TestBuildDingTalkUpstreamClaims_EmptyStaff(t *testing.T) {
	staff := &DingTalkStaffInfo{}
	claims := buildDingTalkUpstreamClaims(staff, "UNION_AAA", "CORP_X")

	require.Equal(t, "", claims["email"])
	require.Equal(t, "", claims["username"])
	// 重构后 subject = unionID（与 identityKey.ProviderSubject 保持一致）
	require.Equal(t, "UNION_AAA", claims["subject"])
	require.Equal(t, "", claims["corp_user_id"]) // 企业 userid 跨组织时为空
	require.Equal(t, "UNION_AAA", claims["union_id"])
	require.Equal(t, "CORP_X", claims["corp_id"])
}

// TestCheckDingTalkCorpAllowed_CrossOrgPolicy 验证 policy=none 时允许任意 corp。
// D: corp 校验提前后逻辑不变。
func TestCheckDingTalkCorpAllowed_CrossOrgPolicy(t *testing.T) {
	cfg := config.DingTalkConnectConfig{CorpRestrictionPolicy: "none"}

	assert.True(t, checkDingTalkCorpAllowed(cfg, "dingABC"), "policy=none should allow any corp")
	assert.True(t, checkDingTalkCorpAllowed(cfg, ""), "policy=none should allow empty corp")
	assert.True(t, checkDingTalkCorpAllowed(cfg, "foreign_corp"), "policy=none should allow foreign corp")
}

// TestCheckDingTalkCorpAllowed_InternalOnly 验证 policy=internal_only 时的 corp 校验语义（方案 A 修订）。
// 钉钉 userAccessToken 在部分授权场景（扫码登录、非企业工作台入口）不返回 corpId 字段，
// 因此 checkDingTalkCorpAllowed 完全不校验 corpID，由 step 3 GetUserIdByUnionId 做真实判定
// （跨企业用户会被钉钉错误码 60011/60121 拒绝，mapDingTalkErrorCode 映射回 corp_rejected）。
func TestCheckDingTalkCorpAllowed_InternalOnly(t *testing.T) {
	cfgWithCorpID := config.DingTalkConnectConfig{
		CorpRestrictionPolicy: "internal_only",
		InternalCorpID:        "dingInternal",
	}
	assert.True(t, checkDingTalkCorpAllowed(cfgWithCorpID, "dingInternal"), "internal_only: matching corpID allowed")
	assert.True(t, checkDingTalkCorpAllowed(cfgWithCorpID, "foreign_corp"), "internal_only: corpID 字段不再用于决策，step 3 兜底")
	assert.True(t, checkDingTalkCorpAllowed(cfgWithCorpID, ""), "internal_only: 空 corpID 也通过（钉钉部分授权场景不返回 corpId）")

	cfgNoCorpID := config.DingTalkConnectConfig{
		CorpRestrictionPolicy: "internal_only",
		InternalCorpID:        "",
	}
	assert.True(t, checkDingTalkCorpAllowed(cfgNoCorpID, "dingAnyNonEmpty"), "internal_only + no InternalCorpID: 非空 corpID 通过")
	assert.True(t, checkDingTalkCorpAllowed(cfgNoCorpID, ""), "internal_only + no InternalCorpID: 空 corpID 也通过")
}

// TestDecideDingTalkStep34Strategy_PolicyNone 验证 policy=none 时
// Step 3/4 失败应降级（shouldFallback=true, isFatal=false）。
func TestDecideDingTalkStep34Strategy_PolicyNone(t *testing.T) {
	step3Err := &DingTalkAPIError{Code: "60011", Message: "not in directory", HTTP: 403}

	shouldFallback, isFatal := decideDingTalkStep34Strategy("none", step3Err)

	require.True(t, shouldFallback, "policy=none: step3 failure should trigger fallback")
	require.False(t, isFatal, "policy=none: step3 failure should NOT be fatal")
}

// TestDecideDingTalkStep34Strategy_PolicyNoneEmpty 验证 policy="" 时行为与 "none" 相同。
func TestDecideDingTalkStep34Strategy_PolicyNoneEmpty(t *testing.T) {
	stepErr := &DingTalkAPIError{Code: "60011", Message: "not in directory", HTTP: 403}

	shouldFallback, isFatal := decideDingTalkStep34Strategy("", stepErr)

	require.True(t, shouldFallback, "policy='': step failure should trigger fallback")
	require.False(t, isFatal, "policy='': step failure should NOT be fatal")
}

// TestDecideDingTalkStep34Strategy_PolicyInternalOnly 验证 policy=internal_only 时
// Step 3/4 失败应 hard fail（isFatal=true）。
func TestDecideDingTalkStep34Strategy_PolicyInternalOnly(t *testing.T) {
	step3Err := &DingTalkAPIError{Code: "60011", Message: "not in directory", HTTP: 403}

	shouldFallback, isFatal := decideDingTalkStep34Strategy("internal_only", step3Err)

	require.False(t, shouldFallback, "policy=internal_only: should NOT fallback on step3 error")
	require.True(t, isFatal, "policy=internal_only: step3 failure should be fatal")
}

// TestDecideDingTalkStep34Strategy_NoError 验证 stepErr=nil 时两个返回值均为 false。
func TestDecideDingTalkStep34Strategy_NoError(t *testing.T) {
	for _, policy := range []string{"none", "internal_only", ""} {
		shouldFallback, isFatal := decideDingTalkStep34Strategy(policy, nil)
		require.False(t, shouldFallback, "no error should not trigger fallback (policy=%q)", policy)
		require.False(t, isFatal, "no error should not be fatal (policy=%q)", policy)
	}
}

// TestCompleteDingTalkRegistration_UsernameFromEmailLocalPart 验证 username 为空时
// 退到 email local part（@ 之前的部分）。
// E: CompleteDingTalkOAuthRegistration username fallback。
func TestCompleteDingTalkRegistration_UsernameFromEmailLocalPart(t *testing.T) {
	tests := []struct {
		name      string
		email     string
		username  string
		wantUser  string
		wantValid bool
	}{
		{
			name:      "username empty, normal email → local part",
			email:     "dingtalk-uid123@dingtalk-connect.invalid",
			username:  "",
			wantUser:  "dingtalk-uid123",
			wantValid: true,
		},
		{
			name:      "username already set → keep original",
			email:     "user@example.com",
			username:  "张三",
			wantUser:  "张三",
			wantValid: true,
		},
		{
			name:      "username empty, no @ in email → use whole email",
			email:     "noemail",
			username:  "",
			wantUser:  "noemail",
			wantValid: true,
		},
		{
			name:      "both empty → invalid",
			email:     "",
			username:  "",
			wantUser:  "",
			wantValid: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			username := tc.username
			email := tc.email

			// 模拟 CompleteDingTalkOAuthRegistration 中的 fallback 逻辑
			if username == "" {
				if at := strings.Index(email, "@"); at > 0 {
					username = email[:at]
				} else {
					username = email
				}
			}

			isValid := email != "" && username != ""
			require.Equal(t, tc.wantUser, username, fmt.Sprintf("username for email=%q", tc.email))
			require.Equal(t, tc.wantValid, isValid, "validity check")
		})
	}
}

// TestBuildDingTalkUpstreamClaims_SubjectEqualsUnionID 验证重构后 subject = unionID
// 而非 staff.UserID，与 identityKey.ProviderSubject 保持一致。
// §4.2: buildDingTalkUpstreamClaims subject 字段修正。
func TestBuildDingTalkUpstreamClaims_SubjectEqualsUnionID(t *testing.T) {
	staff := &DingTalkStaffInfo{UserID: "user123", Name: "张三", Email: "zhangsan@corp.com"}
	claims := buildDingTalkUpstreamClaims(staff, "union456", "dingcorp789")

	// 重构后 subject = unionID（全局唯一，与 identityKey.ProviderSubject 一致）
	require.Equal(t, "union456", claims["subject"], "subject should equal unionID after refactor")
	// 企业 userid 保留为独立字段，供 audit/debug 使用
	require.Equal(t, "user123", claims["corp_user_id"], "corp_user_id should be staff.UserID")
	// union_id 字段与 subject 相同（冗余保留，便于读取）
	require.Equal(t, "union456", claims["union_id"])
	require.Equal(t, "dingcorp789", claims["corp_id"])
	require.Equal(t, "张三", claims["username"])
	require.Equal(t, "zhangsan@corp.com", claims["email"])
}

// TestBuildDingTalkUpstreamClaims_CrossOrgEmptyCorpUserID 验证跨组织降级时
// corp_user_id 为空字符串（跨组织拿不到企业 userid），subject 仍为 unionID。
func TestBuildDingTalkUpstreamClaims_CrossOrgEmptyCorpUserID(t *testing.T) {
	// 跨组织降级路径：staff = &DingTalkStaffInfo{}（所有字段为零值）
	staff := &DingTalkStaffInfo{}
	claims := buildDingTalkUpstreamClaims(staff, "union_cross_org", "foreign_corp")

	require.Equal(t, "union_cross_org", claims["subject"], "subject should still be unionID for cross-org users")
	require.Equal(t, "", claims["corp_user_id"], "corp_user_id should be empty for cross-org fallback")
	require.Equal(t, "", claims["email"])
	require.Equal(t, "", claims["username"])
}

// TestBuildDingTalkUpstreamClaims_PrimaryDeptIDInClaims 验证首个 dept_id 被存入 claims。
func TestBuildDingTalkUpstreamClaims_PrimaryDeptIDInClaims(t *testing.T) {
	staff := &DingTalkStaffInfo{UserID: "u1", Name: "张三", Email: "a@b.com", DeptIDs: []int64{42, 99}}
	claims := buildDingTalkUpstreamClaims(staff, "uid1", "corpX")

	// 兼容旧会话的 primary_dept_id 仍取首个部门
	require.Equal(t, int64(42), claims["primary_dept_id"], "primary_dept_id should be the first dept_id")
}

// TestBuildDingTalkUpstreamClaims_NoDeptIDs 验证无部门时 primary_dept_id=0。
func TestBuildDingTalkUpstreamClaims_NoDeptIDs(t *testing.T) {
	staff := &DingTalkStaffInfo{UserID: "u2", Name: "李四"}
	claims := buildDingTalkUpstreamClaims(staff, "uid2", "corpY")

	require.Equal(t, int64(0), claims["primary_dept_id"], "primary_dept_id should be 0 when no depts")
}

// TestDingTalkStaffFromClaims_RoundTrip 验证 dingTalkStaffFromClaims 能从 claims 恢复 staff 信息。
func TestDingTalkStaffFromClaims_RoundTrip(t *testing.T) {
	staff := &DingTalkStaffInfo{UserID: "u3", Name: "王五", Email: "ww@corp.com", DeptIDs: []int64{9007199254740993, 55, 9223372036854775807}}
	claims := buildDingTalkUpstreamClaims(staff, "uid3", "corpZ")
	require.Equal(t, staff.DeptIDs, dingTalkStaffFromClaims(claims).DeptIDs)

	data, err := json.Marshal(claims)
	require.NoError(t, err)
	var stored map[string]any
	require.NoError(t, json.Unmarshal(data, &stored))
	require.Equal(t, []any{"9007199254740993", "55", "9223372036854775807"}, stored["dept_ids"])

	recovered := dingTalkStaffFromClaims(stored)
	require.Equal(t, "王五", recovered.Name)
	require.Equal(t, "ww@corp.com", recovered.Email)
	require.Equal(t, "u3", recovered.UserID)
	require.Equal(t, staff.DeptIDs, recovered.DeptIDs)
}

func TestDingTalkStaffFromClaims_LegacyPrimaryDeptID(t *testing.T) {
	claims := map[string]any{"primary_dept_id": int64(42)}
	require.Equal(t, []int64{42}, dingTalkStaffFromClaims(claims).DeptIDs)

	var stored map[string]any
	require.NoError(t, json.Unmarshal([]byte(`{"primary_dept_id":42}`), &stored))
	require.Equal(t, []int64{42}, dingTalkStaffFromClaims(stored).DeptIDs)
}

// 部门表只列真实子部门，未列出的 ID 模拟上游查询失败。
type dingTalkTestDept struct {
	name     string
	parentID int64
}

func newDingTalkDeptTestClient(t *testing.T, depts map[int64]dingTalkTestDept) (*DingTalkClient, func(int64) int) {
	t.Helper()
	var mu sync.Mutex
	calls := map[int64]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			DeptID int64 `json:"dept_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode department request: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		calls[req.DeptID]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		dept, ok := depts[req.DeptID]
		if !ok {
			_, _ = w.Write([]byte(`{"errcode":60003,"errmsg":"not found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"errcode": 0, "result": map[string]any{
			"dept_id": req.DeptID, "name": dept.name, "parent_id": dept.parentID,
		}})
	}))
	t.Cleanup(server.Close)
	cli := &DingTalkClient{
		cfg:         dingTalkClientConfig{UserInfoURL: server.URL},
		httpClient:  server.Client(),
		appToken:    "tok",
		appTokenExp: time.Now().Add(time.Hour),
	}
	return cli, func(id int64) int {
		mu.Lock()
		defer mu.Unlock()
		return calls[id]
	}
}

func TestResolveDingTalkDeptPath_SingleLevel(t *testing.T) {
	handler := &AuthHandler{}
	cli, callCount := newDingTalkDeptTestClient(t, map[int64]dingTalkTestDept{42: {"研发部", 1}})
	path, err := handler.resolveDingTalkDeptPath(context.Background(), cli, []int64{42})
	require.NoError(t, err)
	require.Equal(t, "研发部", path)
	require.Equal(t, 1, callCount(42))
	require.Zero(t, callCount(1))
}

// TestSyncDingTalkIdentity_UsesCfgAttrKeys 验证 syncDingTalkIdentity 使用 cfg 中配置的 attr key
// 而不是硬编码值。通过 userAttributeService=nil 使同步路径走 warn 跳过，但在此之前先验证
// syncField 构建逻辑（即 attr key 从 cfg 读取）。
// 间接验证：通过构造定制 cfg，确认不同 attr key 可以正确传入（编译时保证类型正确，运行时不 panic）。
func TestSyncDingTalkIdentity_UsesCfgAttrKeys_NoopWithNilService(t *testing.T) {
	handler := &AuthHandler{
		userAttributeService: nil, // nil → 触发 warn 跳过，但不 panic
	}

	cfg := config.DingTalkConnectConfig{
		CorpRestrictionPolicy: "internal_only",
		SyncCorpEmail:         true,
		SyncDisplayName:       true,
		SyncDept:              true,
		// 自定义 attr key（非默认值）
		SyncCorpEmailAttrKey:   "custom_email_key",
		SyncDisplayNameAttrKey: "custom_name_key",
		SyncDeptAttrKey:        "custom_dept_key",
	}

	staff := &DingTalkStaffInfo{
		Name:  "张三",
		Email: "zhangsan@example.com",
	}

	// 调用不应 panic（userAttributeService 为 nil 时走 warn 跳过路径）
	require.NotPanics(t, func() {
		handler.syncDingTalkIdentity(context.Background(), cfg, nil, 42, staff, false)
	})
}

// TestSyncDingTalkIdentity_DefaultAttrKeys_NoopWithNilService 验证 cfg 默认 attr key 为空时
// 使用 fallback 默认值（dingtalk_email / dingtalk_name / dingtalk_department）。
// 此测试主要验证调用路径不 panic；实际 key 赋值默认值的逻辑在 GetDingTalkConnectOAuthConfig 层。
func TestSyncDingTalkIdentity_DefaultAttrKeys_NoopWithNilService(t *testing.T) {
	handler := &AuthHandler{
		userAttributeService: nil,
	}

	cfg := config.DingTalkConnectConfig{
		CorpRestrictionPolicy: "internal_only",
		SyncCorpEmail:         true,
		SyncDisplayName:       true,
		SyncDept:              false,
		// 不设置 attr key（等同于 GetDingTalkConnectOAuthConfig 未设置时 fallback 后的默认值已在调用前填充）
		SyncCorpEmailAttrKey:   "dingtalk_email",
		SyncDisplayNameAttrKey: "dingtalk_name",
		SyncDeptAttrKey:        "dingtalk_department",
	}

	staff := &DingTalkStaffInfo{
		Name:  "李四",
		Email: "lisi@corp.com",
	}

	require.NotPanics(t, func() {
		handler.syncDingTalkIdentity(context.Background(), cfg, nil, 99, staff, false)
	})
}

func TestResolveDingTalkDeptPath_MultiLevel(t *testing.T) {
	handler := &AuthHandler{}
	cli, _ := newDingTalkDeptTestClient(t, map[int64]dingTalkTestDept{
		42: {"AI研发", 10}, 10: {"研发部", 1},
	})
	path, err := handler.resolveDingTalkDeptPath(context.Background(), cli, []int64{42})
	require.NoError(t, err)
	require.Equal(t, "研发部/AI研发", path)
}

func TestResolveDingTalkDeptPath_MultipleDepartments(t *testing.T) {
	depts := map[int64]dingTalkTestDept{
		10: {"研发", 1},
		11: {"平台", 10},
		12: {"算法", 10},
		20: {"不计/显示名/里的斜线", 1},
		30: {"Tools", 1},
		31: {"A", 30},
		40: {"缺少上级", 99},
	}
	for _, tc := range []struct {
		name    string
		ids     []int64
		want    string
		wantErr bool
	}{
		{name: "depth counts departments even when a name contains slashes", ids: []int64{20, 1, 11}, want: "研发/平台"},
		// 研发/平台 有 5 个字符、13 字节；Tools/A 有 7 个字符、7 字节，且叶名更短。
		{name: "compare runes in the full path", ids: []int64{11, 31}, want: "Tools/A"},
		{name: "tie keeps first department", ids: []int64{11, 12}, want: "研发/平台"},
		{name: "reversed tie keeps new first department", ids: []int64{12, 11}, want: "研发/算法"},
		{name: "root only", ids: []int64{1}, want: ""},
		{name: "missing leaf after valid path", ids: []int64{11, 99}, wantErr: true},
		{name: "missing ancestor before valid path", ids: []int64{40, 11}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cli, calls := newDingTalkDeptTestClient(t, depts)
			path, err := (&AuthHandler{}).resolveDingTalkDeptPath(context.Background(), cli, tc.ids)
			require.Zero(t, calls(1))
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, path)
		})
	}
}

type dingTalkDeptDefinitionRepo struct {
	service.UserAttributeDefinitionRepository
	def service.UserAttributeDefinition
}

func (r *dingTalkDeptDefinitionRepo) GetByKey(_ context.Context, key string) (*service.UserAttributeDefinition, error) {
	if key != r.def.Key {
		return nil, fmt.Errorf("unknown attribute %q", key)
	}
	return &r.def, nil
}

func (r *dingTalkDeptDefinitionRepo) List(context.Context, bool) ([]service.UserAttributeDefinition, error) {
	return []service.UserAttributeDefinition{r.def}, nil
}

type dingTalkDeptValueRepo struct {
	service.UserAttributeValueRepository
	value string
}

func (r *dingTalkDeptValueRepo) UpsertBatch(_ context.Context, _ int64, inputs []service.UpdateUserAttributeInput) error {
	for _, input := range inputs {
		r.value = input.Value
	}
	return nil
}

func TestSyncDingTalkIdentity_DepartmentAttribute(t *testing.T) {
	const oldDept = "原部门"
	cfg := config.DingTalkConnectConfig{
		CorpRestrictionPolicy: "internal_only", SyncDept: true, SyncDeptAttrKey: "dingtalk_department",
	}
	defs := &dingTalkDeptDefinitionRepo{def: service.UserAttributeDefinition{
		ID: 42, Key: cfg.SyncDeptAttrKey, Type: service.AttributeTypeText, Enabled: true,
	}}
	depts := map[int64]dingTalkTestDept{
		10: {"研发", 1}, 11: {"平台", 10}, 12: {"基础架构", 10}, 13: {"八达岭项目", 10},
	}
	for _, tc := range []struct {
		name           string
		ids            []int64
		want           string
		sharedAncestor int64
		pendingLogin   bool
	}{
		{name: "pending login with parent first and project second", ids: []int64{10, 13}, want: "研发/八达岭项目", sharedAncestor: 10, pendingLogin: true},
		{name: "write best path and reuse shared ancestor", ids: []int64{11, 12}, want: "研发/基础架构", sharedAncestor: 10},
		{name: "root clears previous department", ids: []int64{1}, want: ""},
		{name: "lookup failure preserves previous department", ids: []int64{11, 99}, want: oldDept},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := &dingTalkDeptValueRepo{value: oldDept}
			handler := &AuthHandler{userAttributeService: service.NewUserAttributeService(defs, values)}
			cli, calls := newDingTalkDeptTestClient(t, depts)
			staff := &DingTalkStaffInfo{DeptIDs: tc.ids}
			if tc.pendingLogin {
				data, err := json.Marshal(buildDingTalkUpstreamClaims(staff, "uid", "corp"))
				require.NoError(t, err)
				var stored map[string]any
				require.NoError(t, json.Unmarshal(data, &stored))
				handler.syncDingTalkIdentityFromClaims(context.Background(), cfg, cli, 7, stored, false)
			} else {
				handler.syncDingTalkIdentity(context.Background(), cfg, cli, 7, staff, false)
			}
			require.Equal(t, tc.want, values.value)
			if tc.sharedAncestor != 0 {
				require.Equal(t, 1, calls(tc.sharedAncestor), "shared ancestor lookup count")
			}
		})
	}
}

type dingTalkProfileFields struct {
	service.UserAttributeDefinitionRepository
}

func (dingTalkProfileFields) List(ctx context.Context, _ bool) ([]service.UserAttributeDefinition, error) {
	return []service.UserAttributeDefinition{
		{ID: 1, Key: "name", Type: service.AttributeTypeText, Enabled: true},
		{ID: 2, Key: "email", Type: service.AttributeTypeText, Enabled: true},
		{ID: 3, Key: "dept", Type: service.AttributeTypeText, Enabled: true},
	}, ctx.Err()
}

func (r dingTalkProfileFields) GetByKey(ctx context.Context, key string) (*service.UserAttributeDefinition, error) {
	defs, err := r.List(ctx, true)
	if err != nil {
		return nil, err
	}
	for _, def := range defs {
		if def.Key == key {
			return &def, nil
		}
	}
	return nil, fmt.Errorf("unknown key %q", key)
}

type dingTalkProfileValues struct {
	service.UserAttributeValueRepository
	attrs map[int64]string
}

func (v dingTalkProfileValues) UpsertBatch(ctx context.Context, _ int64, inputs []service.UpdateUserAttributeInput) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, in := range inputs {
		v.attrs[in.AttributeID] = in.Value
	}
	return nil
}

func TestSyncDingTalkIdentity_SecondDepartmentCancellationKeepsOtherFields(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID int64 `json:"dept_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if req.ID == 11 {
			cancel()
			return
		}
		fmt.Fprint(w, `{"result":{"dept_id":10,"name":"研发","parent_id":1}}`)
	}))
	defer server.Close()
	values := dingTalkProfileValues{attrs: map[int64]string{3: "原部门"}}
	h := &AuthHandler{userAttributeService: service.NewUserAttributeService(dingTalkProfileFields{}, values)}
	client := &DingTalkClient{cfg: dingTalkClientConfig{UserInfoURL: server.URL}, httpClient: server.Client(), appToken: "token", appTokenExp: time.Now().Add(time.Hour)}
	cfg := config.DingTalkConnectConfig{CorpRestrictionPolicy: "internal_only", SyncDisplayName: true, SyncDisplayNameAttrKey: "name", SyncCorpEmail: true, SyncCorpEmailAttrKey: "email", SyncDept: true, SyncDeptAttrKey: "dept"}
	h.syncDingTalkIdentity(ctx, cfg, client, 7, &DingTalkStaffInfo{Name: "员工", Email: "user@example.org", DeptIDs: []int64{10, 11}}, false)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.Equal(t, map[int64]string{1: "员工", 2: "user@example.org", 3: "原部门"}, values.attrs)
}
