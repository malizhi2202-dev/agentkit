package semantic

import (
	"context"
	"errors"
)

// UnsupportedNotice 是  冻结的「不支持」统一文案（逐字，不得改写）。
const UnsupportedNotice = "暂不支持该诉求"

// ErrNoEntityParser 表示宿主未注入 EntityParser：无法识别实体时**拒绝**继续，
// 而不是把「没注入」静默当作「没有实体」（fail-closed，L-005 / AD-14）。
var ErrNoEntityParser = errors.New("semantic: 未注入 EntityParser，无法识别实体")

// Entity 是**已识别**的取值对象（术语表「实体」，如 `ip:1.2.3.4`）。
//
// JSON 键固定为 type / value ——  断言 `$.data.entities[0]=={type:"ip",value:"1.2.3.4"}`。
// 取值类型（"ip" / "domain" / ...）由注入的 EntityParser 决定：本框架**不复刻**类型枚举，
// 避免与既有 `既有取型实现` 的口径两处定义、各自漂移。
type Entity struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// Intent 是一次输入承载的诉求标识；一次输入**可并存多个**。
//
// 取值开放：只冻结 AC 已钉死的常量，其余由 宿主侧 侧按需扩展，不在框架侧自行发明。
type Intent string

const (
	// IntentCorrelationAnalysis 是「关联分析」诉求。
	IntentCorrelationAnalysis Intent = "correlation_analysis"
	// IntentPreliminaryReport 是「初步报告」诉求。
	IntentPreliminaryReport Intent = "preliminary_report"
)

// Candidate 是**尚未确认**的可能实体：输入含多个实体或类型歧义时给出候选列表，
// 由界面提示待用户确认。
//
// 与 Entity 刻意是不同类型：候选在**编译期**就无法被当作已识别实体送进编排，
// 这是「不擅自选一个执行」的结构侧保障的一半（另一半是 Understanding.Executable）。
// 用户确认某个候选后，其 `{type,value}` 可直接升格为 Entity。
type Candidate struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// Mechanism 是「当前能力范围列表」的一项。
//
// Name 是能力标识（与工具名 / capability 名同口径，供审计与匹配）；
// Label 是面向用户的名称（不支持提示直接展示，前端无需再维护一份标识→中文映射）。
type Mechanism struct {
	Name  string `json:"name"`
	Label string `json:"label"`
}

// Unsupported 是「诉求不被现有能力范围覆盖」的显式结果。
//
// 它是  契约中 unsupported / notice / mechanisms 三个字段的落点：
// 被 Understanding 内嵌后即展平为 `unsupported:true` + `notice` + `mechanisms[]`。
// **只**能由 UnsupportedFor 构造，避免出现「结构存在但判定为 false」的自相矛盾值。
type Unsupported struct {
	Unsupported bool        `json:"unsupported"`
	Notice      string      `json:"notice,omitempty"`
	Mechanisms  []Mechanism `json:"mechanisms,omitempty"`
}

// UnsupportedFor 构造「不支持」结果：判定恒为 true、文案取冻结值、能力范围原样承载。
func UnsupportedFor(mechanisms []Mechanism) Unsupported {
	return Unsupported{Unsupported: true, Notice: UnsupportedNotice, Mechanisms: mechanisms}
}

// IsUnsupported 报告是否被判为不支持。
func (x Unsupported) IsUnsupported() bool { return x.Unsupported }

// Understanding 是语义层对一次输入的完整结果，也是  契约里
// entities / intents / candidates / unsupported / notice / mechanisms 六个字段的落点。
//
// 「是否允许进入编排」只有**一个**判据入口：Executable。
type Understanding struct {
	Entities   []Entity    `json:"entities"`
	Intents    []Intent    `json:"intents"`
	Candidates []Candidate `json:"candidates"`
	Unsupported
}

// Executable 报告该理解结果是否允许进入编排（工具调用）。
//
// 这是「不擅自执行」的**唯一**判据：
//   - 存在待确认候选 ⇒ false：语义层不替用户从候选里挑一个，等用户确认（executed_steps 恒为 0）；
//   - 诉求不被能力范围覆盖 ⇒ false：不猜测执行（executed_steps 恒为 0）。
func (u Understanding) Executable() bool {
	return len(u.Candidates) == 0 && !u.IsUnsupported()
}

// HasIntent 报告诉求集合是否包含 want —— 即  的 `intents ⊇ {...}` 断言口径，
// 也是引擎按诉求装配工具的依据。
func (u Understanding) HasIntent(want Intent) bool {
	for _, got := range u.Intents {
		if got == want {
			return true
		}
	}
	return false
}

// EntityParser 是宿主注入的实体识别器：宿主侧 侧注入既有
// `既有取型实现(ctx, value) (string, *errors.Error)` 的适配器（，零改动既有实现）。
//
// 签名只用 stdlib `context.Context`：本包 不得出现 magdefined.Context（ADR-004 / 零依赖红线）。
//
// 契约（下游适配器必须遵守）：
//   - 「识别不出」⇒ 返回**空类型 + nil error**。「不是实体」是正常结论（如 `asdf@@@` 走澄清），
//     不得当作故障；既有 GetValueType 用 `宿主错误包.NotHaveRes` 表达这一类结论，适配器需把它映射为空类型。
//   - 返回非 nil error ⇒ 识别过程**真实失败**（如下游不可达），由调用方按降级 / 缺口口径处置，不得吞掉。
type EntityParser interface {
	EntityType(ctx context.Context, value string) (string, error)
}

// ParseEntities 用注入的 EntityParser 把一组取值解析为实体。
//
// 契约：
//   - 识别不出的取值被**跳过**，不产出 `{type:"",value:x}` 这类伪实体；
//   - 保持输入顺序 ⇒ 同一输入恒定输出（确定性，利于  的结构一致性）；
//   - 识别过程真实失败上抛，不静默当作「不是实体」；
//   - 未注入解析器 ⇒ fail-closed（ErrNoEntityParser），不因「没配就没限制」而放行。
func ParseEntities(ctx context.Context, parser EntityParser, values []string) ([]Entity, error) {
	if parser == nil {
		return nil, ErrNoEntityParser
	}
	var entities []Entity
	for _, value := range values {
		entityType, err := parser.EntityType(ctx, value)
		if err != nil {
			return nil, err
		}
		if entityType == "" {
			continue
		}
		entities = append(entities, Entity{Type: entityType, Value: value})
	}
	return entities, nil
}
