package apikey

import "github.com/target/goalert/permission"

// GQLPolicy is a GraphQL API key policy.
type GQLPolicy struct {
	Version       int
	AllowedFields []string
	Role          permission.Role
}
