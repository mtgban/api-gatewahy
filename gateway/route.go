package gateway

import (
	"errors"
	"regexp"
	"strings"
)

// ErrNoRoute means the path is not one the gateway forwards.
var ErrNoRoute = errors.New("no such route")

// Route is a parsed public path.
type Route struct {
	Game string
	Kind string
	Rest string
}

var gameName = regexp.MustCompile(`^[a-z0-9]+$`)

// ParseRoute splits /v1/{game}/mtgban/... into its parts.
func ParseRoute(path string) (Route, error) {
	rest, ok := strings.CutPrefix(path, "/v1/")
	if !ok {
		return Route{}, ErrNoRoute
	}
	// A segment carrying .. could climb out of the backend's /api prefix.
	for _, seg := range strings.Split(rest, "/") {
		if strings.Contains(seg, "..") {
			return Route{}, ErrNoRoute
		}
	}
	game, rest, ok := strings.Cut(rest, "/")
	if !ok || !gameName.MatchString(game) {
		return Route{}, ErrNoRoute
	}
	sub, ok := strings.CutPrefix(rest, "mtgban/")
	if !ok || sub == "" {
		return Route{}, ErrNoRoute
	}
	r := Route{Game: game, Rest: rest}
	switch {
	case strings.HasPrefix(sub, "search/"):
		r.Kind = "search"
	case sub == "sets.json", sub == "sets.csv", sub == "stores.json", sub == "stores.csv":
		r.Kind = "meta"
	default:
		for _, kind := range []string{"retail", "buylist", "all", "sealed"} {
			if sub == kind+".json" || sub == kind+".csv" || strings.HasPrefix(sub, kind+"/") {
				r.Kind = kind
				break
			}
		}
	}
	if r.Kind == "" {
		return Route{}, ErrNoRoute
	}
	return r, nil
}

// BackendPath is the path on the game host.
func (r Route) BackendPath() string {
	return "/api/" + r.Rest
}

// NeedsModes lists the modes the plan must include for this route.
func (r Route) NeedsModes() []string {
	switch r.Kind {
	case "all":
		return []string{"retail", "buylist"}
	case "retail", "buylist", "sealed":
		return []string{r.Kind}
	}
	return nil
}
