// Package ghx is the read cache of gh-throttle. Its classification
// (allowlist.go), output shaping (shape.go) and cache-key context (context.go)
// are ported unchanged, apart from the package name, from ghx by Bruno Borges
// (https://github.com/brunoborges/ghx, MIT License, see LICENSE in this
// directory). store.go and run.go replace ghx's in-memory daemon with an
// on-disk store shared by every gh process, keeping ghx's caching rules.
package ghx
