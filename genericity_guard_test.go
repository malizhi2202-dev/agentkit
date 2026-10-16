package agentkit

// 用户裁决（2026-10-16）：agentkit 是**通用包**，唯一事实就是本目录；不得出现任何与宿主业务
// 相关的设计、文档、规范编号与措辞。宿主业务工件（分层盘点 / 实施计划 / 验收报告 / 产品原型）
// 留在宿主的 .specs/ 下，不进本包。本文件把这条要求变成会红的判据，防止它再次被"顺手带进来"。
import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// hostBusinessTokens 用于非 .go 工件（README / docs / samples / tools）：命中即失败，零容忍。
var hostBusinessTokens = regexp.MustCompile(`(?i)宿主服务|宿主服务|研判|资产|画像|whois|推推|tuitui|` +
	`asset|10\.0\.0\.8|AC-2\.7|UAT-[0-9]|T-L[0-9]|logic/engine|controllers/engine|` +
	`qihoo|corp\.`)

// goBanTokens 用于 .go：字面量匹配（不用正则 ⇒ 没有转义陷阱；本文件此前两次正是被引号写坏的）。
// 三类：① 宿主业务词与平台名；② 宿主规范编号前缀（需求条目号、任务号、宿主设计文档段落号）；
// ③ 宿主源码目录。通用包可以陈述自己的不变量，但不许引用某一家产品的需求编号 —— 那等于把
// 宿主的规范体系搬进公共包。包自身的 import 路径（宿主 module 前缀）在扫描前剔除：
// 同仓不分家是位置事实，不是业务污染。
var goBanTokens = []string{
	"whois", "推推", "tuitui", "宿主服务", "研判", "资产", "画像", "引擎响应契约",
	"v3-1", "V3", "Asset", "asset", "DESIGN §", "§", "UAT-",
	"logic/", "controllers/", "pkg/cloudweb/", "errorx", "10.0.0.8",
	// 内网标识与宿主产品名（推公网后不可回潮）。
	"qihoo", "宿主内网前缀-", "goweb", "corp.", "Heimdallr", "heimdallr", "海姆达尔", "pkg/agentkit",
	// 卡片线格式属平台协议：这些键只能在接入方出现（cmd/im-gateway/card_codec.go）。
	"bgcolor", "msgtype", "interactive", "/hm/", "X-Hm-User", "X-HM-User", "msgtype",
}

// goRoleRef 抓被引号包住的宿主角色名，避免误伤普通文本里的 v3。
var goRoleRef = regexp.MustCompile(`"v3"`)

// goAcRef 抓 AC-1.2-3 这类宿主需求编号；要求"AC-"后紧跟数字 ⇒ 不误伤 HMAC-SHA256 之类的正常词。
var goAcRef = regexp.MustCompile(`AC-[0-9]`)

// goTnnRef 抓 T03 / T12 这类两位数字的宿主任务号。
var goTnnRef = regexp.MustCompile(`\bT[0-9]{2}\b`)

func goBusinessHit(line string) string {
	for _, tok := range goBanTokens {
		if strings.Contains(line, tok) {
			return tok
		}
	}
	if m := goRoleRef.FindString(line); m != "" {
		return m
	}
	if m := goAcRef.FindString(line); m != "" {
		return m + "…"
	}
	return goTnnRef.FindString(line)
}

// .go 侧同为零容忍（2026-10-16 清零后棘轮退役）。

// selfFiles 是禁令的载体：这些文件里的字面量是"禁令本身"，不算污染。
func selfFiles(name string) bool {
	return name == "genericity_guard_test.go" || name == "guard_test.go"
}

// modulePrefix 返回**本包自身的 module 路径**，扫描前先剔除：那是本包的身份，不是宿主业务。
// 逐层向上找最近的 go.mod —— 在宿主仓里它是宿主 module（本包为其 `本包` 子树），
// 独立成模块后它就是本包自己的 module（原先写死 `../../go.mod` 只适配前者，会让本包
// 在独立模块里读不到 go.mod ⇒ 前缀按空处理 ⇒ 自己的 import 行被企业内网禁词误判）。
func modulePrefix(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Logf("取不到工作目录，module 前缀按空处理：%v", err)
		return ""
	}
	for i := 0; i < 8; i++ {
		data, rerr := os.ReadFile(filepath.Join(dir, "go.mod"))
		if rerr == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "module ") {
					return strings.Fields(line)[1]
				}
			}
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// TestGuard_GenericArtifactsHaveNoHostBusiness：包内非 .go 工件必须零宿主业务标识。
func TestGuard_GenericArtifactsHaveNoHostBusiness(t *testing.T) {
	var bad []string
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if strings.HasSuffix(path, ".go") || selfFiles(info.Name()) {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		// 与 .go 侧同一口径：非 .go 工件也要先剔除**本包自身的 module 路径**，
		// 否则独立成模块后 go.mod 与文档里的 import 行会被企业内网禁词误判为宿主业务
		// ——那是本包的身份，不是宿主的业务。
		body := strings.ReplaceAll(string(data), modulePrefix(t), "")
		for i, line := range strings.Split(body, "\n") {
			if m := hostBusinessTokens.FindString(line); m != "" {
				bad = append(bad, fmt.Sprintf("%s:%d 出现宿主业务标识 %q", path, i+1, m))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Fatalf("通用包内出现宿主业务（%d 处）：\n  %s\n⇒ 宿主业务的设计与文档必须留在宿主 .specs/，本包只放通用内容",
			len(bad), strings.Join(bad, "\n  "))
	}
}

// TestGuard_GoDocHostBusinessRatchet：.go 里的宿主词表 / 规范编号 / 宿主目录 / 产品措辞只许降不许升。
func TestGuard_GoCodeHasNoHostBusiness(t *testing.T) {
	prefix := modulePrefix(t)
	var bad []string
	_ = filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || selfFiles(info.Name()) {
			return err
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		body := strings.ReplaceAll(string(data), prefix, "") // 剔除自身 import 路径
		for i, line := range strings.Split(body, "\n") {
			if m := goBusinessHit(line); m != "" {
				bad = append(bad, fmt.Sprintf("%s:%d 命中禁项 %q", path, i+1, m))
			}
		}
		return nil
	})
	if len(bad) > 0 {
		t.Fatalf("通用包 .go 内仍有宿主业务或宿主规范编号（%d 处）：\n  %s\n⇒ 机制归包、平台细节与需求编号归接入方",
			len(bad), strings.Join(bad, "\n  "))
	}
}
