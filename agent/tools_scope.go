package agent

import "github.com/Autumn-27/artex/db"

// taskScopeOverview exposes the current task's actual range, even when coverage
// metrics are disabled. A company ID alone is not enough for planning: its live
// rules include targets and discovery keywords that may not have any assets yet.
// Source tasks keep their own scope under related_tasks, preserving provenance.
func (t *ToolSet) taskScopeOverview() ([]map[string]any, error) {
	rows, err := t.as.ListTaskScope(t.taskID)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		entry := map[string]any{"kind": row.Kind, "source": row.Source}
		if row.Reason != "" {
			entry["reason"] = row.Reason
		}
		switch {
		case row.Domain != "":
			entry["value"] = row.Domain
		case row.Net != "":
			entry["value"] = row.Net
		case row.Value != "":
			entry["value"] = row.Value
		case row.CompanyID != nil:
			entry["company_id"] = *row.CompanyID
			entry["company_name"] = row.CompanyName
			if t.cs != nil {
				rules, err := t.cs.GetScope(*row.CompanyID)
				if err != nil {
					return nil, err
				}
				scope := make([]map[string]any, 0, len(rules))
				keywords := make([]string, 0)
				for _, rule := range rules {
					value := companyScopeDisplayValue(rule)
					scope = append(scope, map[string]any{"kind": rule.Kind, "value": value})
					if rule.Kind == "keyword" {
						keywords = append(keywords, value)
					}
				}
				entry["company_scope"] = scope
				entry["company_keywords"] = keywords
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

// Keep the user's original spelling (especially company names and ICP numbers)
// in the agent context. Older rules without raw text fall back to stored values.
func companyScopeDisplayValue(rule db.ScopeRule) string {
	if rule.Raw != "" {
		return rule.Raw
	}
	if rule.Domain != "" {
		return rule.Domain
	}
	if rule.Net != "" {
		return rule.Net
	}
	return rule.Value
}
