# Changelog

## 0.1.2

- Skip files whose live content already matches the new desired state
  during apply (e.g. sops re-encryption with unchanged plaintext) —
  avoids spurious writes and false restart-required flags.

## 0.1.1

- Fix: s6-overlay v3 strips the container environment for CMD — read
  SUPERVISOR_TOKEN from /run/s6/container_environment/ as a fallback and
  pass HOME explicitly to sops. Fixes 401s on every core API call and
  sops home-directory warnings.

## 0.1.0

- Initial v0: apply loop (sparse checkout, sops decrypt, config check
  with rollback, targeted reload), Tier-1 drift detection, ingress panel
  with per-file diff and promote/revert, drift sensor, conflict
  surfacing. First run baselines without writing.
