# Contributing

Contributions are welcome. Bug reports, feature requests, and pull requests all go through [GitHub issues](https://github.com/kanya-approve/karpenter-provider-rackspace-spot/issues) and pull requests on the same repository.

## Pull requests

Keep PRs focused — one logical change per PR. Squash messy intermediate commits before requesting review; the final history should read as a series of meaningful, atomic changes. Reviewers will look at the diff against `main`, not the per-commit history.

A good PR includes:

- A short description of what changed and why
- Reproduction steps or a test for any bug fix
- Updates to the relevant docs (README, CRD field comments, etc.)
- All checks green (`make build`, `make test`, `make chart-lint`)

## Development

Build, test, and lint all run from the Makefile:

```sh
make generate       # regenerate CRDs + deepcopy; sync chart crds/
make build          # build the controller binary
make test           # unit tests
make chart-lint     # helm lint
make chart-template # render the chart with default values
```

Run `make help` for the full target list.

## Code style

- Go: `gofmt` is enforced. Run `gofmt -w ./...` before committing.
- Comments: explain the *why* when behavior is non-obvious; identifiers should carry the *what*.
- Tests: prefer mocks from `spot-go-sdk/api/v1/mocks` over hand-rolled fakes for the SDK surface.

## Code of conduct

By participating, you agree to abide by the [Code of Conduct](CODE_OF_CONDUCT.md).
