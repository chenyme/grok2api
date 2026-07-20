package model

// ClientModelAlias 是下游客户端可见的兼容模型名。
// 用于 Cursor 等拒绝带点号模型 ID（如 grok-4.5）的场景。
type ClientModelAlias struct {
	// Alias 是客户端可填写、并会出现在 GET /v1/models 中的名称。
	Alias string
	// Target 是真实对外模型名（无 Provider 前缀），例如 grok-4.5。
	Target string
}

// ClientModelAliases 返回需要在模型列表中额外暴露的兼容名称。
func ClientModelAliases() []ClientModelAlias {
	return []ClientModelAlias{
		{Alias: "grok-4-5", Target: "grok-4.5"},
	}
}
