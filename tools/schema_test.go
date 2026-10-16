package tools

import (
	"errors"
	"reflect"
	"testing"
)

type schemaIn struct {
	IP      string   `json:"ip"`
	Ports   []int    `json:"ports,omitempty"`
	Verbose bool     `json:"verbose,omitempty"`
	Note    string   `json:"-"`
	ImpAct  string   `json:"imp_actor"`
	Tags    []string `json:"tags"`
	hidden  string
}

type nestedBad struct {
	Inner schemaIn `json:"inner"`
}

type mapBad struct {
	Extra map[string]string `json:"extra"`
}

type sliceStructBad struct {
	Items []schemaIn `json:"items"`
}

type embeddedBad struct {
	schemaIn
	Extra string `json:"extra"`
}

type exportedEmbeddedBad struct {
	Exported schemaIn
	Extra    string `json:"extra"`
}

type onlyUnexported struct{ hidden string }

func props(t *testing.T, sch map[string]any) map[string]any {
	t.Helper()
	p, ok := sch["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties 缺失或形状不对：%v", sch["properties"])
	}
	return p
}

func TestTools_SchemaForShape(t *testing.T) {
	sch, err := SchemaFor(schemaIn{})
	if err != nil {
		t.Fatalf("SchemaFor：%v", err)
	}
	if sch["type"] != "object" || sch["additionalProperties"] != false {
		t.Fatalf("顶层形状应为 object 且不接受多余键：%v", sch)
	}
	p := props(t, sch)
	if _, leaked := p["Note"]; leaked {
		t.Fatal(`json:"-" 的字段不得进 schema`)
	}
	if _, leaked := p["hidden"]; leaked {
		t.Fatal("未导出字段不得进 schema")
	}
	if len(p) != 5 {
		t.Fatalf("可导出字段应全在且只这些：%v", keysOf(p))
	}
	if got := p["ip"]; !reflect.DeepEqual(got, map[string]any{"type": "string"}) {
		t.Fatalf("ip 形状：%v", got)
	}
	if got := p["ports"]; !reflect.DeepEqual(got, map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}) {
		t.Fatalf("ports 形状：%v", got)
	}
	if got := p["verbose"]; !reflect.DeepEqual(got, map[string]any{"type": "boolean"}) {
		t.Fatalf("verbose 形状：%v", got)
	}
	want := []string{"imp_actor", "ip", "tags"}
	if got := RequiredFromSchema(sch); !reflect.DeepEqual(got, want) {
		t.Fatalf("required 应为「无 omitempty 的字段」且字典序：期望 %v 实得 %v", want, got)
	}
	// 指针样例同样可用（只用类型不用值）
	if _, err := SchemaFor(&schemaIn{}); err != nil {
		t.Fatalf("指针样例应被接受：%v", err)
	}
	// RequiredFromSchema 交出副本：改它不影响 schema
	r := RequiredFromSchema(sch)
	r[0] = "tampered"
	if RequiredFromSchema(sch)[0] != "imp_actor" {
		t.Fatal("required 交出的是引用而非副本")
	}
}

func TestTools_SchemaForFailsClosed(t *testing.T) {
	for _, c := range []struct {
		name    string
		sample  any
		wantErr error
	}{
		{"非结构体：整数", 42, ErrSchemaUnsupported},
		{"非结构体：map", map[string]string{}, ErrSchemaUnsupported},
		{"非结构体：切片", []schemaIn{}, ErrSchemaUnsupported},
		{"非结构体：nil", nil, ErrSchemaUnsupported},
		{"嵌套结构体字段", nestedBad{}, ErrSchemaUnsupported},
		{"map 字段", mapBad{}, ErrSchemaUnsupported},
		{"结构体切片字段", sliceStructBad{}, ErrSchemaUnsupported},
		{"内嵌结构体（未导出类型）", embeddedBad{}, ErrSchemaUnsupported},
		{"内嵌结构体（导出类型）", exportedEmbeddedBad{}, ErrSchemaUnsupported},
		{"无可导出字段", onlyUnexported{}, ErrSchemaEmpty},
	} {
		if _, err := SchemaFor(c.sample); !errors.Is(err, c.wantErr) {
			t.Errorf("%s：应判 %v，实得 %v", c.name, c.wantErr, err)
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
