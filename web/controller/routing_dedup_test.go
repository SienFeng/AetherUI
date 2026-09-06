package controller

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"a-ui/database"
	"a-ui/database/model"
	"a-ui/util/common"
	"a-ui/web/service"
)

// 没有去掉任何重复时，响应必须与改动前逐字节一致。绝大多数保存都走这一支，
// 挂一句「已自动去掉 0 条重复」既是噪音，也会让管理员怀疑自己填错了。
func TestDomainGroupSaveResponseUnchangedWhenNothingDeduplicated(t *testing.T) {
	gin.SetMode(gin.TestMode)

	baseline := httptest.NewRecorder()
	c1, _ := gin.CreateTestContext(baseline)
	jsonMsg(c1, "添加域名组", nil)

	got := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(got)
	jsonMsgDedup(c2, "添加域名组", 0, 0, nil)

	if got.Body.String() != baseline.Body.String() {
		t.Errorf("body = %s, want %s（无重复时响应必须与 jsonMsg 一致）",
			got.Body.String(), baseline.Body.String())
	}
}

// 保存失败时绝不能捎带去重说明：这一次根本没落库，说「已自动去掉 3 条重复」
// 会让管理员以为库里的内容已经被改过了。
func TestDomainGroupSaveErrorResponseIgnoresDedupCount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	failure := common.NewError("boom")

	baseline := httptest.NewRecorder()
	c1, _ := gin.CreateTestContext(baseline)
	jsonMsg(c1, "添加域名组", failure)

	got := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(got)
	jsonMsgDedup(c2, "添加域名组", 3, 2, failure)

	if got.Body.String() != baseline.Body.String() {
		t.Errorf("body = %s, want %s（失败路径不得带去重说明）",
			got.Body.String(), baseline.Body.String())
	}
}

// 域名与 IP 段分开报：两个输入框是分开的，只给一个总数的话管理员不知道该去
// 哪个框看自己写了什么。
func TestDomainGroupSaveResponseReportsBothKindsSeparately(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	jsonMsgDedup(c, "修改域名组", 2, 1, nil)

	var msg struct {
		Success bool   `json:"success"`
		Msg     string `json:"msg"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, rec.Body.String())
	}
	if !msg.Success {
		t.Fatalf("success = false, body = %s", rec.Body.String())
	}
	if !strings.HasPrefix(msg.Msg, "修改域名组成功") {
		t.Errorf("msg = %q, 必须以原来那句「修改域名组成功」开头", msg.Msg)
	}
	for _, want := range []string{"2 条重复域名", "1 条重复 IP 段"} {
		if !strings.Contains(msg.Msg, want) {
			t.Errorf("msg = %q, 缺少 %q", msg.Msg, want)
		}
	}
}

// 只有一类有重复时，不能连另一类也一起报出来。
func TestDomainGroupSaveResponseOmitsTheKindWithNoDuplicates(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	jsonMsgDedup(c, "添加域名组", 0, 4, nil)

	var msg struct {
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(msg.Msg, "4 条重复 IP 段") {
		t.Errorf("msg = %q, 缺少 IP 段的条数", msg.Msg)
	}
	// 断言「重复域名」而不是「域名」：动作名「添加域名组」本身就含「域名」。
	if strings.Contains(msg.Msg, "重复域名") {
		t.Errorf("msg = %q, 域名一条都没去掉，不该出现在提示里", msg.Msg)
	}
}

// 这条守的是「接线」：ParseDomains/ParseCidrs 已经在数去掉了几条，但只要
// controller 没把那两个数字传给 jsonMsgDedup，提示就永远不会出现——而保存
// 照常成功、去重也照常发生，jsonMsgDedup 自己那几条单元测试还全绿。整条链路
// 里没有任何一层会报错，只有管理员再也看不到自己的输入被改过。
func TestAddDomainGroupReportsDeduplicatedCounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}

	body := url.Values{
		"remark": {"测速网"},
		// domain:A.com 归一后与第一行相同，是管理员自己看不出来的那类重复。
		"domains": {"domain:a.com\ndomain:A.com\ndomain:b.com"},
		"cidrs":   {"8.8.8.8\n8.8.8.8"},
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req

	a := &RoutingController{}
	a.addDomainGroup(c)

	var msg struct {
		Success bool   `json:"success"`
		Msg     string `json:"msg"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, rec.Body.String())
	}
	if !msg.Success {
		t.Fatalf("保存失败了，后面的断言无意义: %s", rec.Body.String())
	}
	for _, want := range []string{"1 条重复域名", "1 条重复 IP 段"} {
		if !strings.Contains(msg.Msg, want) {
			t.Errorf("msg = %q, 缺少 %q", msg.Msg, want)
		}
	}

	// 落库的必须是去重后的内容，提示与实际写入不能各说各话。
	groups, err := (&service.DomainGroupService{}).GetAll()
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("len(groups) = %d, want 1", len(groups))
	}
	if groups[0].Domains != `["domain:a.com","domain:b.com"]` {
		t.Errorf("Domains = %s, want [\"domain:a.com\",\"domain:b.com\"]", groups[0].Domains)
	}
	if groups[0].Cidrs != `["8.8.8.8"]` {
		t.Errorf("Cidrs = %s, want [\"8.8.8.8\"]", groups[0].Cidrs)
	}
}

// 编辑走的是另一个 handler。只在新建那条路径上接线的话，管理员在既有组里
// 粘进重复内容照样被静默去重，而这个包里其余的去重测试全都是绿的。
func TestUpdateDomainGroupReportsDeduplicatedCounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	groupService := &service.DomainGroupService{}
	group := &model.DomainGroup{Remark: "测速网", Domains: `["domain:a.com"]`, Cidrs: "[]"}
	if err := groupService.Add(group); err != nil {
		t.Fatalf("Add: %v", err)
	}

	body := url.Values{
		"remark":  {"测速网"},
		"domains": {"domain:a.com\ndomain:a.com"},
		"cidrs":   {"8.8.8.8\n1.1.1.1\n8.8.8.8"},
	}
	req := httptest.NewRequest("POST", "/", strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = req
	c.Params = gin.Params{{Key: "id", Value: strconv.Itoa(group.Id)}}

	a := &RoutingController{}
	a.updateDomainGroup(c)

	var msg struct {
		Success bool   `json:"success"`
		Msg     string `json:"msg"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("unmarshal: %v, body = %s", err, rec.Body.String())
	}
	if !msg.Success {
		t.Fatalf("保存失败了，后面的断言无意义: %s", rec.Body.String())
	}
	for _, want := range []string{"1 条重复域名", "1 条重复 IP 段"} {
		if !strings.Contains(msg.Msg, want) {
			t.Errorf("msg = %q, 缺少 %q", msg.Msg, want)
		}
	}
}
