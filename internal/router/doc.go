// Package router turns a configured set of (host, path-prefix) rules into a
// lookup structure that answers "which upstream serves this request?" in O(k)
// where k is the depth of the path prefix.
//
// The router is immutable after build. Config reloads construct a fresh
// router and swap the atomic pointer in internal/proxy. That means the hot
// path reads are lock-free and the write side never blocks requests.
package router
