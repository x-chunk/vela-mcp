package mcpserver

type schema = map[string]any

func object(properties schema, required ...string) schema {
	s := schema{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}
func str(description string) schema { return schema{"type": "string", "description": description} }
func enum(description string, values ...string) schema {
	return schema{"type": "string", "description": description, "enum": values}
}
func integer(description string, min, max int64) schema {
	s := schema{"type": "integer", "description": description, "minimum": min}
	if max > 0 {
		s["maximum"] = max
	}
	return s
}
func boolean(description string) schema { return schema{"type": "boolean", "description": description} }
func array(items schema, description string) schema {
	return schema{"type": "array", "items": items, "description": description}
}
func nullable(s schema) schema { return schema{"anyOf": []any{s, schema{"type": "null"}}} }
func filePath() schema {
	return str("Logical Vela file path, e.g. docs/report.md. Not a local filesystem path.")
}
func dirPath() schema {
	return str("Logical directory prefix, e.g. docs/. Empty string or / means the root.")
}
func rev() schema {
	return integer("Apply only to this file/directory revision. Reuse a revision from an earlier result; inspect only when unknown or stale.", 1, 0)
}
func id() schema {
	return schema{"type": "string", "pattern": "^[0-9a-hjkmnp-tv-z]{26}$", "description": "Vela lowercase ULID from a previous result."}
}
func metadata() schema {
	return object(schema{
		"visibility":   enum("Default for a new file: private. Public is listed; unlisted is downloadable by anyone with its URL.", "public", "unlisted", "private"),
		"title":        str("Display title (max 256 characters); empty clears it."),
		"description":  str("Description (max 4096 characters); empty clears it."),
		"tags":         array(str("Tag: letters, digits, dash, underscore, dot; normalized to lowercase."), "Replace all tags, max 32. Empty array clears tags."),
		"content_type": str("MIME type, e.g. text/markdown. Omit to detect; text input defaults to text/plain; charset=utf-8."),
		"disposition":  enum("Whether browsers display the file or download it.", "inline", "attachment"),
		"expires_at":   str("Future RFC3339 time or Vela duration such as 7d. 'none' clears an expiry during replacement."),
	})
}
func metadataPatch() schema {
	props := metadata()["properties"].(schema)
	for k, v := range props {
		props[k] = nullable(v.(schema))
	}
	props["expires_at"] = nullable(str("Future RFC3339 timestamp, or null to remove expiry. Relative durations are accepted only when uploading/creating."))
	s := object(props)
	s["minProperties"] = 1
	return s
}
func directoryPatch() schema {
	s := object(schema{
		"index":   nullable(enum("Enable listing or inherit the parent policy.", "on", "off", "inherit")),
		"hidden":  nullable(boolean("Hide this entrance from its parent index. This does not protect file contents.")),
		"noindex": nullable(enum("Crawler noindex/nofollow hints; this does not enforce privacy.", "on", "off", "inherit")),
		"title":   nullable(str("Directory display title.")), "description": nullable(str("Directory display description.")),
	})
	s["minProperties"] = 1
	return s
}
func pagination(props schema) schema {
	props["limit"] = integer("Page size, default 50. Request another page only if needed.", 1, 1000)
	props["cursor"] = str("Opaque next_cursor from the previous page. Keep the same filters; omit to start again.")
	return object(props)
}
