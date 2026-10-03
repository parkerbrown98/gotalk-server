package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/parkerbrown98/gotalk-server/internal/service"
)

const tagModeration = "Moderation"

type ReportPath struct {
	PlacePath
	ReportID string `path:"reportID" format:"uuid"`
}

type CreateReportRequest struct {
	PostID  string `json:"post_id,omitempty" format:"uuid" doc:"Report a post (set exactly one of post_id or user_id)"`
	UserID  string `json:"user_id,omitempty" format:"uuid" doc:"Report a member"`
	Reason  string `json:"reason" enum:"spam,harassment,inappropriate,off_topic,other"`
	Details string `json:"details,omitempty" maxLength:"2000"`
}

type ResolveReportRequest struct {
	Status         string `json:"status" enum:"resolved,dismissed"`
	ResolutionNote string `json:"resolution_note,omitempty" maxLength:"512"`
}

type TimeoutRequest struct {
	Duration int64  `json:"duration" minimum:"60" maximum:"2419200" doc:"Seconds (1 minute to 28 days)"`
	Reason   string `json:"reason,omitempty" maxLength:"512"`
}

type WarningRequest struct {
	Reason string `json:"reason" minLength:"1" maxLength:"512"`
}

func (s *Server) registerModeration() {
	huma.Register(s.api, withContentRateLimit(withStatus(withAuth(operation("create-report", http.MethodPost,
		"/places/{place}/reports", "Report a post or member to the place's moderators", tagModeration)), http.StatusCreated)),
		handle(s, func(ctx context.Context, in *struct {
			PlacePath
			Body CreateReportRequest
		}) (*Body[Report], error) {
			postID, err := parseOptionalID("post_id", in.Body.PostID)
			if err != nil {
				return nil, err
			}
			userID, err := parseOptionalID("user_id", in.Body.UserID)
			if err != nil {
				return nil, err
			}
			r, err := s.Service.CreateReport(ctx, mustPrincipal(ctx), in.Place, service.ReportInput{
				PostID: postID, UserID: userID, Reason: in.Body.Reason, Details: in.Body.Details,
			})
			if err != nil {
				return nil, err
			}
			return ok(reportForReporter(r))
		}))

	huma.Register(s.api, withAuth(operation("list-reports", http.MethodGet, "/places/{place}/reports",
		"Moderation queue, newest first (MANAGE_REPORTS)", tagModeration)),
		handle(s, func(ctx context.Context, in *struct {
			PlacePath
			PageQuery
			Status string `query:"status" enum:"open,resolved,dismissed"`
		}) (*Body[Page[Report]], error) {
			page := in.pagination()
			reports, err := s.Service.ListReports(ctx, mustPrincipal(ctx), in.Place, in.Status, page)
			if err != nil {
				return nil, err
			}
			return ok(pageOf(reports, toReport, page))
		}))

	huma.Register(s.api, withAuth(operation("resolve-report", http.MethodPatch, "/places/{place}/reports/{reportID}",
		"Resolve or dismiss a report (MANAGE_REPORTS)", tagModeration)),
		handle(s, func(ctx context.Context, in *struct {
			ReportPath
			Body ResolveReportRequest
		}) (*Body[Report], error) {
			id, err := parseID("reportID", in.ReportID)
			if err != nil {
				return nil, err
			}
			r, err := s.Service.ResolveReport(ctx, mustPrincipal(ctx), in.Place, id, in.Body.Status, in.Body.ResolutionNote)
			if err != nil {
				return nil, err
			}
			return ok(toReport(r))
		}))

	huma.Register(s.api, withAuth(operation("list-audit-log", http.MethodGet, "/places/{place}/audit-log",
		"Moderation and administration history, newest first (VIEW_AUDIT_LOG)", tagModeration)),
		handle(s, func(ctx context.Context, in *struct {
			PlacePath
			PageQuery
			Action   string `query:"action" maxLength:"64" doc:"Exact action (member.ban) or category (member)"`
			ActorID  string `query:"actor_id" format:"uuid"`
			TargetID string `query:"target_id" format:"uuid" doc:"e.g. a user ID to see their moderation history"`
		}) (*Body[Page[AuditEntry]], error) {
			actor, err := parseOptionalID("actor_id", in.ActorID)
			if err != nil {
				return nil, err
			}
			target, err := parseOptionalID("target_id", in.TargetID)
			if err != nil {
				return nil, err
			}
			page := in.pagination()
			entries, err := s.Service.ListAuditLog(ctx, mustPrincipal(ctx), in.Place,
				service.AuditFilter{Action: in.Action, ActorID: actor, TargetID: target}, page)
			if err != nil {
				return nil, err
			}
			return ok(pageOf(entries, toAuditEntry, page))
		}))

	huma.Register(s.api, withAuth(operation("timeout-member", http.MethodPut, "/places/{place}/members/{userID}/timeout",
		"Time out a member: they can read but not post, reply or react (MODERATE_MEMBERS)", tagModeration)),
		handle(s, func(ctx context.Context, in *struct {
			MemberPath
			Body TimeoutRequest
		}) (*Body[Member], error) {
			id, err := parseID("userID", in.UserID)
			if err != nil {
				return nil, err
			}
			m, err := s.Service.TimeoutMember(ctx, mustPrincipal(ctx), in.Place, id,
				time.Duration(in.Body.Duration)*time.Second, in.Body.Reason)
			if err != nil {
				return nil, err
			}
			return ok(toMember(m))
		}))

	huma.Register(s.api, withStatus(withAuth(operation("clear-member-timeout", http.MethodDelete,
		"/places/{place}/members/{userID}/timeout", "End a member's timeout early (MODERATE_MEMBERS)", tagModeration)), http.StatusOK),
		handle(s, func(ctx context.Context, in *MemberPath) (*Body[Member], error) {
			id, err := parseID("userID", in.UserID)
			if err != nil {
				return nil, err
			}
			m, err := s.Service.ClearTimeout(ctx, mustPrincipal(ctx), in.Place, id)
			if err != nil {
				return nil, err
			}
			return ok(toMember(m))
		}))

	huma.Register(s.api, withStatus(withAuth(operation("warn-member", http.MethodPost,
		"/places/{place}/members/{userID}/warnings", "Send a member a formal warning (MODERATE_MEMBERS)", tagModeration)), http.StatusNoContent),
		handle(s, func(ctx context.Context, in *struct {
			MemberPath
			Body WarningRequest
		}) (*struct{}, error) {
			id, err := parseID("userID", in.UserID)
			if err != nil {
				return nil, err
			}
			return nil, s.Service.WarnMember(ctx, mustPrincipal(ctx), in.Place, id, in.Body.Reason)
		}))
}
