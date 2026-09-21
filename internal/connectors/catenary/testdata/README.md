# `provision-v1.openapi.yaml` — provenance

Vendored byte-for-byte (verified with `git hash-object`, not retyped or
reformatted) from:

- **Source repository:** `catenary` (github.com/Einlanzerous/catenary)
- **Path:** `provision/openapi.yaml`
- **Tag:** `provision-v1`
- **Blob hash:** `f58acacb9ab9938d31fe98a4513d4b427d516225`

Reproduce with:

```sh
git -C <catenary-checkout> show provision-v1:provision/openapi.yaml > provision-v1.openapi.yaml
git -C <catenary-checkout> rev-parse provision-v1:provision/openapi.yaml   # f58acacb9ab9938d31fe98a4513d4b427d516225
```

This is Catenary's hand-written provisioning contract (PRSR-50's brief, CANT-131).
It is not part of Catenary's generated wire schema and carries no conformance
vectors there; Catenary's own `server/spec/provision_test.go` is what keeps it
honest against the real handlers. Here it backs `contract_test.go`, which
validates every request the connector sends and every response the test double
emits against this document, so the double can't silently drift from what
`provision-v1` actually promises.

A breaking change upstream is `provision-v2` and a second tag, never an edit
under this one (the source file's own `info.description` says so) — so this
file should only ever be replaced wholesale, with the tag and hash above
updated to match, never hand-edited.
