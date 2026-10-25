// Package docs holds no code. Its tests guard the Markdown under docs/ and the
// repo-root READMEs: every relative link resolves, every #anchor names a heading,
// and docs/README.md indexes every page (ADR-0026). The docs live beside the code
// so they change in the same PR as the behaviour they describe; these tests are
// what stops a moved or renamed page from leaving a dead link behind.
package docs
