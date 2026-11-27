# Verification fixture

A tiny project that Osmia's tests use to prove the verification lever against a
real running service. It is not a product.

- `app/server.js` is the service. It shows an instance token, a random value
  generated when the service starts, and keeps notes in memory.
- `main.go` is the lever: a Dagger module whose `verify` function starts the
  service anew for a run ID and drives it with the shared
  [web-driver](../../modules/web-driver) module. `list` returns the named flows
  as JSON.
- `docs/verification` is the feature map.
- `check.sh` runs the lever for real and checks the reports. The
  `web-driver-fixture` Dagger check runs it.

The lever takes the run ID as a constructor argument, before the function:

```sh
dagger call verification-fixture list
dagger call verification-fixture --run-id <run ID> verify --flow=home-page export --path <dir>
dagger call verification-fixture --run-id <run ID> verify '--steps=goto notes; snapshot' export --path <dir>
```

Give `verify` either a named flow or an ad-hoc step list, whose syntax is in the
[module's README](../../modules/web-driver/README.md). The module is a local
dependency, so run the fixture from a checkout of the Osmia repository. A
product installs the module by git ref instead.
