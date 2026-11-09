package scim

// discovery answers GET /ServiceProviderConfig, /ResourceTypes and /Schemas (RFC 7644 §4): what
// Quai supports, and the attributes it keeps, which are all it shows.
func (h *Handler) discovery(what string) any {
	switch what {
	case "ServiceProviderConfig":
		return map[string]any{
			"schemas":          []string{schemaConfig},
			"documentationUri": "https://github.com/damhau/quai/blob/main/docs/design/08-identity-access-tenancy.md#scim-provisioning",
			"patch":            map[string]any{"supported": true},
			"bulk":             map[string]any{"supported": false, "maxOperations": 0, "maxPayloadSize": 0},
			"filter":           map[string]any{"supported": true, "maxResults": maxCount},
			"changePassword":   map[string]any{"supported": false},
			"sort":             map[string]any{"supported": false},
			"etag":             map[string]any{"supported": false},
			"authenticationSchemes": []map[string]any{{"type": "oauthbearertoken", "name": "SCIM token",
				"description": "A bearer token of auth.scim.tokens (quai scim-token)", "primary": true}},
			"meta": map[string]any{"resourceType": "ServiceProviderConfig", "location": h.base + "/ServiceProviderConfig"},
		}
	case "ResourceTypes":
		return list(2, 0, []map[string]any{
			{"schemas": []string{schemaType}, "id": "User", "name": "User", "endpoint": "/Users", "schema": schemaUser,
				"schemaExtensions": []map[string]any{{"schema": schemaEnterprise, "required": false}},
				"meta":             map[string]any{"resourceType": "ResourceType", "location": h.base + "/ResourceTypes/User"}},
			{"schemas": []string{schemaType}, "id": "Group", "name": "Group", "endpoint": "/Groups", "schema": schemaGroup,
				"meta": map[string]any{"resourceType": "ResourceType", "location": h.base + "/ResourceTypes/Group"}},
		})
	}
	attr := func(name, typ string, required bool, uniqueness string) map[string]any {
		return map[string]any{"name": name, "type": typ, "multiValued": false, "required": required, "caseExact": false,
			"mutability": "readWrite", "returned": "default", "uniqueness": uniqueness}
	}
	ref := func(name string, multi bool) map[string]any {
		return map[string]any{"name": name, "type": "complex", "multiValued": multi, "required": false, "mutability": "readWrite",
			"returned": "default", "subAttributes": []map[string]any{attr("value", "string", true, "none")}}
	}
	schema := func(id, name string, attrs ...map[string]any) map[string]any {
		return map[string]any{"schemas": []string{schemaSchema}, "id": id, "name": name, "attributes": attrs,
			"meta": map[string]any{"resourceType": "Schema", "location": h.base + "/Schemas/" + id}}
	}
	return list(3, 0, []map[string]any{
		schema(schemaUser, "User", attr("userName", "string", true, "server"), attr("externalId", "string", true, "server"),
			attr("active", "boolean", false, "none")),
		schema(schemaEnterprise, "EnterpriseUser", ref("manager", false)),
		schema(schemaGroup, "Group", attr("displayName", "string", true, "server"), attr("externalId", "string", false, "server"),
			ref("members", true)),
	})
}
