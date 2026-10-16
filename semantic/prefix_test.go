package semantic

import (
	"encoding/json"
	"reflect"
	"testing"
)

// ：`入口命令: 1.2.3.4` → parsed{cmd:"research",value:"1.2.3.4"}，且无 conflict 键。
func TestParsePrefix_Research(t *testing.T) {
	got, explicit := ParsePrefix("research: 1.2.3.4")
	if !explicit {
		t.Fatalf("explicit = false, want true")
	}
	want := Parsed{Cmd: CmdResearch, Value: "1.2.3.4"}
	if got != want {
		t.Fatalf("ParsePrefix() = %+v, want %+v", got, want)
	}
	if got.Conflict != "" {
		t.Fatalf("Conflict = %q, want empty (契约点 深等值要求无冲突时不出现该键)", got.Conflict)
	}
}

// ：`search: 1.2.3.4` → cmd=="search"。
func TestParsePrefix_Search(t *testing.T) {
	got, explicit := ParsePrefix("search: 1.2.3.4")
	if !explicit {
		t.Fatalf("explicit = false, want true")
	}
	want := Parsed{Cmd: CmdSearch, Value: "1.2.3.4"}
	if got != want {
		t.Fatalf("ParsePrefix() = %+v, want %+v", got, want)
	}
}

// ：中文冒号与半角冒号同结果（含带/不带分隔空格两种写法）。
func TestParsePrefix_ColonCompat(t *testing.T) {
	inputs := []string{
		"research: 1.2.3.4",
		"research:1.2.3.4",
		"research：1.2.3.4",
		"research：1.2.3.4",
	}
	want, explicit := ParsePrefix(inputs[0])
	if !explicit {
		t.Fatalf("baseline explicit = false, want true")
	}
	for _, in := range inputs[1:] {
		got, ok := ParsePrefix(in)
		if !ok {
			t.Fatalf("ParsePrefix(%q) explicit = false, want true", in)
		}
		if got != want {
			t.Fatalf("ParsePrefix(%q) = %+v, want %+v（中英文冒号必须同结果）", in, got, want)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ParsePrefix(%q) deep-unequal to %+v", in, want)
		}
	}
}

// ：`search: 帮我看下这个IP` → 以显式指令为准（cmd=="search"）+ conflict 提示非空，不静默改判。
func TestParsePrefix_ConflictHonorsExplicit(t *testing.T) {
	got, explicit := ParsePrefix("search: 帮我看下这个IP")
	if !explicit {
		t.Fatalf("explicit = false, want true")
	}
	if got.Cmd != CmdSearch {
		t.Fatalf("Cmd = %q, want %q（不得静默改判）", got.Cmd, CmdSearch)
	}
	if got.Value != "帮我看下这个IP" {
		t.Fatalf("Value = %q, want %q", got.Value, "帮我看下这个IP")
	}
	if got.Conflict == "" {
		t.Fatalf("Conflict 为空；契约点 要求卡片不一致提示文案非空")
	}
}

// 对称性：入口命令前缀 + 自然语言诉求同样置 conflict，且指令仍为显式值。
func TestParsePrefix_ConflictIsSymmetric(t *testing.T) {
	got, explicit := ParsePrefix("research: 帮我看下这个IP")
	if !explicit {
		t.Fatalf("explicit = false, want true")
	}
	if got.Cmd != CmdResearch {
		t.Fatalf("Cmd = %q, want %q", got.Cmd, CmdResearch)
	}
	if got.Conflict == "" {
		t.Fatalf("Conflict 为空；宿主入口词前缀遇自然语言诉求也应提示不一致")
	}
}

// 深等值前提：无冲突时 JSON 里只有 cmd / value 两个键（conflict 走 omitempty）。
func TestParsed_JSONContract(t *testing.T) {
	p := Parsed{Cmd: CmdResearch, Value: "1.2.3.4"}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	const wantJSON = `{"cmd":"research","value":"1.2.3.4"}`
	if string(raw) != wantJSON {
		t.Fatalf("Marshal = %s, want %s", raw, wantJSON)
	}

	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	wantMap := map[string]any{"cmd": "research", "value": "1.2.3.4"}
	if !reflect.DeepEqual(m, wantMap) {
		t.Fatalf("parsed = %v, want %v（不得出现 command / content / conflict 键）", m, wantMap)
	}
}

// 有冲突时 conflict 必须真出现在 JSON 里（前端卡片靠它渲染提示文案）。
func TestParsed_JSONConflictPresent(t *testing.T) {
	p := Parsed{Cmd: CmdSearch, Value: "帮我看下这个IP", Conflict: conflictNotice("搜索")}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if m["conflict"] == "" || m["conflict"] == nil {
		t.Fatalf("conflict 未出现在 JSON 中：%s", raw)
	}
}

// 无前缀：不识别为指令，Value 为去空白后的原文，Cmd 留空交调用方判定。
func TestParsePrefix_NoPrefix(t *testing.T) {
	inputs := []string{
		"1.2.3.4",
		"帮我看下这台 MID 的设备档案",
		"asdf@@@",
		"",
	}
	for _, in := range inputs {
		got, explicit := ParsePrefix(in)
		if explicit {
			t.Fatalf("ParsePrefix(%q) explicit = true, want false", in)
		}
		if got.Cmd != "" {
			t.Fatalf("ParsePrefix(%q) Cmd = %q, want empty", in, got.Cmd)
		}
		if got.Conflict != "" {
			t.Fatalf("ParsePrefix(%q) Conflict = %q, want empty", in, got.Conflict)
		}
	}
	if got, _ := ParsePrefix("  1.2.3.4  "); got.Value != "1.2.3.4" {
		t.Fatalf("Value = %q, want trimmed %q", got.Value, "1.2.3.4")
	}
}

// 值本身含冒号（URL / IPv6）不得被误判为前缀指令。
func TestParsePrefix_ColonInsideValueIsNotAPrefix(t *testing.T) {
	inputs := []string{
		"http://1.2.3.4/x",
		"2001:db8::1",
		"宿主入口词分析: 1.2.3.4",
		"搜索一下: 1.2.3.4",
	}
	for _, in := range inputs {
		if _, explicit := ParsePrefix(in); explicit {
			t.Fatalf("ParsePrefix(%q) explicit = true, want false（仅识别 宿主入口词 / 搜索 两个关键字）", in)
		}
	}
}

// 前缀前导空白应被容忍，结果与无空白写法一致。
func TestParsePrefix_TrimsOuterSpace(t *testing.T) {
	got, explicit := ParsePrefix("  research: 1.2.3.4  ")
	if !explicit {
		t.Fatalf("explicit = false, want true")
	}
	want := Parsed{Cmd: CmdResearch, Value: "1.2.3.4"}
	if got != want {
		t.Fatalf("ParsePrefix() = %+v, want %+v", got, want)
	}
}

// 前缀存在但内容为空：不是「矛盾」（缺失 ≠ 矛盾），由调用方决定澄清；不得崩、不得改判。
func TestParsePrefix_EmptyPayload(t *testing.T) {
	for _, in := range []string{"research:", "search：", "research:    "} {
		got, explicit := ParsePrefix(in)
		if !explicit {
			t.Fatalf("ParsePrefix(%q) explicit = false, want true", in)
		}
		if got.Value != "" {
			t.Fatalf("ParsePrefix(%q) Value = %q, want empty", in, got.Value)
		}
		if got.Conflict != "" {
			t.Fatalf("ParsePrefix(%q) Conflict = %q, want empty（缺失内容不是矛盾）", in, got.Conflict)
		}
	}
}

// 确定性：同一输入重复解析结果恒定（不依赖时间 / 随机 / 外部状态）。
func TestParsePrefix_Deterministic(t *testing.T) {
	first, firstOK := ParsePrefix("search: 帮我看下这个IP")
	for i := 0; i < 100; i++ {
		got, ok := ParsePrefix("search: 帮我看下这个IP")
		if ok != firstOK || got != first {
			t.Fatalf("第 %d 次解析 = (%+v,%v), 首轮 = (%+v,%v)", i, got, ok, first, firstOK)
		}
	}
}
