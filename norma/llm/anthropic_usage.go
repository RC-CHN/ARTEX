package llm

// Usage frames are cumulative snapshots. Pointers distinguish an omitted field
// from an explicit zero, including gateways that replace initial estimates.
type anthropicUsage struct {
	InputTokens              *int `json:"input_tokens"`
	OutputTokens             *int `json:"output_tokens"`
	CacheReadInputTokens     *int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens *int `json:"cache_creation_input_tokens"`
	BillingUsage             *struct {
		Semantic    string `json:"semantic"`
		Estimated   bool   `json:"estimated"`
		OpenAIUsage *struct {
			PromptTokens         *int `json:"prompt_tokens"`
			CompletionTokens     *int `json:"completion_tokens"`
			PromptCacheHitTokens *int `json:"prompt_cache_hit_tokens"`
			PromptTokensDetails  struct {
				CachedTokens         *int `json:"cached_tokens"`
				CacheWriteTokens     *int `json:"cache_write_tokens"`
				CachedCreationTokens *int `json:"cached_creation_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"openai_usage"`
	} `json:"billing_usage"`
}

type anthropicMeter struct {
	input, output, read, write int
	inputIncludesCache         bool
}

func (m *anthropicMeter) update(u *anthropicUsage) {
	if u == nil {
		return
	}
	set := func(dst *int, src *int) {
		if src != nil && *src >= 0 {
			*dst = *src
		}
	}
	if u.InputTokens != nil && *u.InputTokens >= 0 {
		m.input, m.inputIncludesCache = *u.InputTokens, false
	}
	set(&m.read, u.CacheReadInputTokens)
	set(&m.write, u.CacheCreationInputTokens)
	if u.OutputTokens != nil {
		m.output = max(m.output, *u.OutputTokens)
	}
	// New API includes the original metering and its explicit semantics. Its
	// converted input_tokens may still include cache hits. Use this declaration,
	// never a model-name/URL heuristic, to avoid counting the cache twice.
	b := u.BillingUsage
	if b == nil || b.Estimated || b.Semantic != "openai" || b.OpenAIUsage == nil {
		return
	}
	o := b.OpenAIUsage
	if o.PromptTokens == nil || *o.PromptTokens < 0 {
		return
	}
	m.input, m.inputIncludesCache = *o.PromptTokens, true
	if o.PromptTokensDetails.CachedTokens != nil {
		set(&m.read, o.PromptTokensDetails.CachedTokens)
	} else {
		set(&m.read, o.PromptCacheHitTokens)
	}
	if o.PromptTokensDetails.CacheWriteTokens != nil {
		set(&m.write, o.PromptTokensDetails.CacheWriteTokens)
	} else {
		set(&m.write, o.PromptTokensDetails.CachedCreationTokens)
	}
	if o.CompletionTokens != nil {
		m.output = max(m.output, *o.CompletionTokens)
	}
}

func (m anthropicMeter) usage() Usage {
	input := m.input
	if !m.inputIncludesCache {
		input += m.read + m.write
	}
	return Usage{InputTokens: input, OutputTokens: m.output, CacheReadTokens: m.read, CacheWriteTokens: m.write}
}

func (u anthropicUsage) norm() Usage {
	var m anthropicMeter
	m.update(&u)
	return m.usage()
}
