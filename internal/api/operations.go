package api

import (
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/parkerbrown98/gotalk-server/internal/apperr"
	"github.com/parkerbrown98/gotalk-server/internal/ratelimit"
)

var (
	secured      = []map[string][]string{{"bearer": {}}}
	optionalAuth = []map[string][]string{{"bearer": {}}, {}}
)

// Operation metadata keys read by middleware.
const (
	rateLimitTierKey = "rateLimitTier"
	sessionOnlyKey   = "sessionOnly"
)

func operation(id, method, path, summary string, tag string) huma.Operation {
	op := huma.Operation{
		OperationID: id,
		Method:      method,
		Path:        path,
		Summary:     summary,
		Tags:        []string{tag},
	}
	switch method {
	case http.MethodPost:
		op.DefaultStatus = http.StatusOK
	case http.MethodDelete:
		op.DefaultStatus = http.StatusNoContent
	}
	return op
}

func withAuth(op huma.Operation) huma.Operation {
	op.Security = secured
	op.Errors = append(op.Errors, http.StatusUnauthorized)
	return op
}

func withOptionalAuth(op huma.Operation) huma.Operation {
	op.Security = optionalAuth
	return op
}

func withStatus(op huma.Operation, status int) huma.Operation {
	op.DefaultStatus = status
	return op
}

func withMeta(op huma.Operation, key string, v any) huma.Operation {
	m := maps.Clone(op.Metadata)
	if m == nil {
		m = map[string]any{}
	}
	m[key] = v
	op.Metadata = m
	return op
}

func withAuthRateLimit(op huma.Operation) huma.Operation {
	return withMeta(op, rateLimitTierKey, ratelimit.TierAuth)
}

func withContentRateLimit(op huma.Operation) huma.Operation {
	return withMeta(op, rateLimitTierKey, ratelimit.TierContent)
}

func withChatRateLimit(op huma.Operation) huma.Operation {
	return withMeta(op, rateLimitTierKey, ratelimit.TierChat)
}

// withSessionOnly keeps API tokens and bots away from an operation: it manages the
// account's credentials or must be done by the person themselves.
func withSessionOnly(op huma.Operation) huma.Operation {
	op.Description = strings.TrimSpace(op.Description + " Requires a login session; API tokens and bots cannot call it.")
	return withMeta(op, sessionOnlyKey, true)
}

func parseOptionalID(field, v string) (*uuid.UUID, error) {
	if v == "" {
		return nil, nil
	}
	id, err := parseID(field, v)
	return &id, err
}

func parseOptionalTime(field, v string) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, apperr.Invalid("%s must be an RFC 3339 timestamp", field)
	}
	return &t, nil
}

func parseOptionalBool(v string) *bool {
	if v == "" {
		return nil
	}
	b := v == "true"
	return &b
}

func parseID(field, v string) (uuid.UUID, error) {
	id, err := uuid.Parse(v)
	if err != nil {
		return uuid.Nil, apperr.Invalid("%s must be a UUID", field)
	}
	return id, nil
}
