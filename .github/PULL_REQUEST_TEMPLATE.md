## What changes

<!-- One or two sentences. Give a concrete example of the behaviour before and after. -->

## Compatibility

<!-- Answer each question. See CONTRIBUTING.md. -->

- Does this change a file format, configuration key, the object-key grammar, or the wire protocol?
- Does any golden test or conformance vector change? If yes, why is the new output right?
- Does a Fleet Manager deployment template grant anything new, or does `fleet-manager/src/terraform_test.go` change? If yes, what may the runtime identity or a reader now do?

## Security

<!-- State the security effect, or write "none". -->

## Checklist

- [ ] `make check` passes, and `make -C fleet-manager check` when `fleet-manager/` changed
- [ ] Fixtures and test data are synthetic
- [ ] No new dependency, or the dependency is argued above
- [ ] Documentation is updated where behaviour changed
