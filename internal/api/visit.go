package api

import (
	"context"
	"strings"

	"deploy_platform/internal/customdomain"
	"deploy_platform/internal/edge"
	"deploy_platform/internal/store"
)

const (
	primaryKindCustom   = "custom"
	primaryKindPlatform = "platform"
)

func pickVisitURL(customHost, platformURL string, platformEnabled bool) (visit, kind string) {
	if host := strings.ToLower(strings.TrimSpace(customHost)); host != "" {
		return customdomain.PublicURL(host), primaryKindCustom
	}
	if platformEnabled {
		return strings.TrimSpace(platformURL), primaryKindPlatform
	}
	return "", ""
}

func (s *Server) platformNodeURL(node store.Node, named []edge.Named) string {
	return s.publicNodeURL(node, named)
}

func (s *Server) applyVisitURLs(ctx context.Context, nodes []store.Node) []store.Node {
	if len(nodes) == 0 {
		return nodes
	}
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node.Kind == "service" && strings.TrimSpace(node.ID) != "" {
			ids = append(ids, node.ID)
		}
	}
	primaries := map[string]string{}
	if s.store != nil {
		if found, err := s.store.PrimaryCustomHosts(ctx, ids); err == nil {
			primaries = found
		}
	}
	named := s.namedServices(ctx, nodes)
	out := append([]store.Node(nil), nodes...)
	for i := range out {
		platform := s.platformNodeURL(out[i], named)
		live := !out[i].PlatformURLRevoked
		out[i].PlatformURLEnabled = live
		if live && platform != "" {
			out[i].PlatformURL = platform
		} else {
			out[i].PlatformURL = ""
		}
		visit, kind := pickVisitURL(primaries[out[i].ID], platform, live)
		out[i].PrimaryKind = kind
		if visit != "" {
			out[i].URL = &visit
		} else if out[i].Kind == "service" {
			out[i].URL = nil
		}
	}
	return out
}

func (s *Server) visitNodeURL(ctx context.Context, node store.Node, named []edge.Named) string {
	if s == nil || s.store == nil {
		visit, _ := pickVisitURL("", s.platformNodeURL(node, named), !node.PlatformURLRevoked)
		return visit
	}
	applied := s.applyVisitURLs(ctx, []store.Node{node})
	if len(applied) == 0 {
		return ""
	}
	return deref(applied[0].URL)
}
