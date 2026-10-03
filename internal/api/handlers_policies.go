package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/parkerbrown98/gotalk-server/internal/service"
)

const tagPolicies = "Policies & transparency"

type PolicyPath struct {
	Kind string `path:"kind" enum:"terms,privacy,guidelines"`
}

type PolicyVersionPath struct {
	PolicyPath
	Version int32 `path:"version" minimum:"1"`
}

type PublishPolicyRequest struct {
	Title           string     `json:"title,omitempty" maxLength:"200" doc:"Defaults to the policy's usual name"`
	Content         string     `json:"content" minLength:"1" maxLength:"200000" doc:"Markdown"`
	Summary         string     `json:"summary,omitempty" maxLength:"2000" doc:"What changed since the previous version"`
	RequiresConsent bool       `json:"requires_consent,omitempty" doc:"Users must accept this version; clients show it until they do"`
	EffectiveAt     *time.Time `json:"effective_at,omitempty" doc:"Schedule the version (up to a year ahead); defaults to now"`
}

type ConsentRequest struct {
	Purpose       string `json:"purpose" minLength:"2" maxLength:"32" pattern:"^[a-z][a-z0-9_]+$" doc:"A policy kind (terms, privacy, guidelines) or another purpose such as analytics"`
	Granted       bool   `json:"granted" doc:"false withdraws consent"`
	PolicyVersion *int32 `json:"policy_version,omitempty" minimum:"1" doc:"For policies: the version accepted; defaults to the current one"`
}

type TransparencyQuery struct {
	Since string `query:"since" format:"date-time" doc:"Start of the period (RFC 3339); defaults to 30 days before until"`
	Until string `query:"until" format:"date-time" doc:"End of the period (RFC 3339); defaults to now. Periods are at most 366 days."`
}

func (q TransparencyQuery) window() (*time.Time, *time.Time, error) {
	since, err := parseOptionalTime("since", q.Since)
	if err != nil {
		return nil, nil, err
	}
	until, err := parseOptionalTime("until", q.Until)
	return since, until, err
}

func (s *Server) registerPolicies() {
	huma.Register(s.api, operation("list-policies", http.MethodGet, "/policies",
		"List the policies in effect (terms, privacy, guidelines)", tagPolicies),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[[]PolicySummary], error) {
			docs, err := s.Service.CurrentPolicies(ctx)
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(docs, toPolicySummary))
		}))

	huma.Register(s.api, operation("get-policy", http.MethodGet, "/policies/{kind}",
		"Get the version of a policy in effect", tagPolicies),
		handle(s, func(ctx context.Context, in *PolicyPath) (*Body[Policy], error) {
			doc, err := s.Service.GetPolicy(ctx, in.Kind)
			if err != nil {
				return nil, err
			}
			return ok(toPolicy(doc))
		}))

	huma.Register(s.api, operation("list-policy-versions", http.MethodGet, "/policies/{kind}/versions",
		"A policy's changelog: every version, newest first, including scheduled ones", tagPolicies),
		handle(s, func(ctx context.Context, in *PolicyPath) (*Body[[]PolicySummary], error) {
			docs, err := s.Service.ListPolicyVersions(ctx, in.Kind)
			if err != nil {
				return nil, err
			}
			return ok(mapSlice(docs, toPolicySummary))
		}))

	huma.Register(s.api, operation("get-policy-version", http.MethodGet, "/policies/{kind}/versions/{version}",
		"Get one version of a policy", tagPolicies),
		handle(s, func(ctx context.Context, in *PolicyVersionPath) (*Body[Policy], error) {
			doc, err := s.Service.GetPolicyVersion(ctx, in.Kind, in.Version)
			if err != nil {
				return nil, err
			}
			return ok(toPolicy(doc))
		}))

	huma.Register(s.api, withStatus(withAuth(operation("publish-policy", http.MethodPost, "/policies/{kind}",
		"Publish a new version of a policy (instance administrators)", tagPolicies)), http.StatusCreated),
		handle(s, func(ctx context.Context, in *struct {
			PolicyPath
			Body PublishPolicyRequest
		}) (*Body[Policy], error) {
			b := in.Body
			doc, err := s.Service.PublishPolicy(ctx, mustPrincipal(ctx), in.Kind, service.PolicyInput{
				Title: b.Title, Content: b.Content, Summary: b.Summary, RequiresConsent: b.RequiresConsent, EffectiveAt: b.EffectiveAt,
			})
			if err != nil {
				return nil, err
			}
			return ok(toPolicy(doc))
		}))

	huma.Register(s.api, withAuth(operation("get-my-consents", http.MethodGet, "/users/@me/consents",
		"The caller's latest consent per purpose and the policies still awaiting consent", tagPolicies)),
		handle(s, func(ctx context.Context, _ *struct{}) (*Body[ConsentStatus], error) {
			st, err := s.Service.GetConsentStatus(ctx, mustPrincipal(ctx))
			if err != nil {
				return nil, err
			}
			return ok(ConsentStatus{Consents: mapSlice(st.Consents, toConsent), Outstanding: mapSlice(st.Outstanding, toPolicySummary)})
		}))

	huma.Register(s.api, withAuth(operation("list-my-consent-history", http.MethodGet, "/users/@me/consents/history",
		"Every consent the caller gave or withdrew, newest first", tagPolicies)),
		handle(s, func(ctx context.Context, in *PageQuery) (*Body[Page[Consent]], error) {
			page := in.pagination()
			records, err := s.Service.ListConsentHistory(ctx, mustPrincipal(ctx), page)
			if err != nil {
				return nil, err
			}
			return ok(pageOf(records, toConsent, page))
		}))

	huma.Register(s.api, withSessionOnly(withStatus(withAuth(operation("record-consent", http.MethodPost, "/users/@me/consents",
		"Give or withdraw consent; records are append-only", tagPolicies)), http.StatusCreated)),
		handle(s, func(ctx context.Context, in *Body[ConsentRequest]) (*Body[Consent], error) {
			rec, err := s.Service.RecordConsent(ctx, mustPrincipal(ctx), service.ConsentInput{
				Purpose: in.Body.Purpose, Granted: in.Body.Granted, PolicyVersion: in.Body.PolicyVersion,
			}, clientFrom(ctx))
			if err != nil {
				return nil, err
			}
			return ok(toConsent(rec))
		}))

	huma.Register(s.api, operation("get-transparency-report", http.MethodGet, "/transparency",
		"Instance-wide moderation statistics: reports by reason and status, actions taken, content removed", tagPolicies),
		handle(s, func(ctx context.Context, in *TransparencyQuery) (*Body[TransparencyReport], error) {
			since, until, err := in.window()
			if err != nil {
				return nil, err
			}
			r, err := s.Service.InstanceTransparency(ctx, since, until)
			if err != nil {
				return nil, err
			}
			return ok(toTransparency(r))
		}))

	huma.Register(s.api, withOptionalAuth(operation("get-place-transparency-report", http.MethodGet, "/places/{place}/transparency",
		"Moderation statistics for a place, readable by anyone who can see the place", tagPolicies)),
		handle(s, func(ctx context.Context, in *struct {
			PlacePath
			TransparencyQuery
		}) (*Body[TransparencyReport], error) {
			since, until, err := in.window()
			if err != nil {
				return nil, err
			}
			r, err := s.Service.PlaceTransparency(ctx, principalFrom(ctx), in.Place, since, until)
			if err != nil {
				return nil, err
			}
			return ok(toTransparency(r))
		}))
}
