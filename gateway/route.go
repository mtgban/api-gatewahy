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
	// Rest is the path under the backend's /api/: mtgban/... for v1, and
	// v2/... for v2.
	Rest string
}

var gameName = regexp.MustCompile(`^[a-z0-9]+$`)

// ParseRoute splits /v1/{game}/... and /v2/{game}/... into their parts,
// forwarded to the backend's /api/mtgban/ and /api/v2/. v2 serves the
// prices and the set and store lists, not search, which the backends answer
// in v1 only.
func ParseRoute(path string) (Route, error) {
	var rest string
	var v2 bool
	switch {
	case strings.HasPrefix(path, "/v1/"):
		rest = path[len("/v1/"):]
	case strings.HasPrefix(path, "/v2/"):
		rest, v2 = path[len("/v2/"):], true
	default:
		return Route{}, ErrNoRoute
	}
	// A segment carrying .. could climb out of the backend's /api prefix.
	for _, seg := range strings.Split(rest, "/") {
		if strings.Contains(seg, "..") {
			return Route{}, ErrNoRoute
		}
	}
	game, sub, ok := strings.Cut(rest, "/")
	if !ok || !gameName.MatchString(game) || sub == "" {
		return Route{}, ErrNoRoute
	}
	r := Route{Game: game, Rest: "mtgban/" + sub}
	if v2 {
		r.Rest = "v2/" + sub
	}
	switch {
	case strings.HasPrefix(sub, "search/"):
		if v2 {
			return Route{}, ErrNoRoute
		}
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
