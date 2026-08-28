# Changelog

## [0.17.0](https://github.com/Einlanzerous/purser/compare/v0.16.0...v0.17.0) (2026-08-28)


### Features

* **cfaccess:** report a launcher mark that a square tile will crop (PRSR-43) ([#54](https://github.com/Einlanzerous/purser/issues/54)) ([c2a8a7f](https://github.com/Einlanzerous/purser/commit/c2a8a7f834022a69d2b0b245ef748d41ca25801e))
* **cfdns:** refuse an out-of-zone hostname before creating anything (PRSR-39) ([#53](https://github.com/Einlanzerous/purser/issues/53)) ([6e6d4f5](https://github.com/Einlanzerous/purser/commit/6e6d4f5d19ebe524d4f18d29d58e7dd0b0e4e5b0))

## [0.16.0](https://github.com/Einlanzerous/purser/compare/v0.15.0...v0.16.0) (2026-08-26)


### Features

* **spinup:** Cloudflare Access application provisioner (PRSR-29) ([#42](https://github.com/Einlanzerous/purser/issues/42)) ([19e9965](https://github.com/Einlanzerous/purser/commit/19e9965e3f87faca0df7ae7be508941dd71602c3))
* **spinup:** provision-service CLI + HTTP surface (PRSR-31) ([#45](https://github.com/Einlanzerous/purser/issues/45)) ([3a35a04](https://github.com/Einlanzerous/purser/commit/3a35a046a325699e2f934197a5dada9ad63738b7))
* **spinup:** resolve the launcher icon from Placard (PRSR-37) ([#50](https://github.com/Einlanzerous/purser/issues/50)) ([adf6390](https://github.com/Einlanzerous/purser/commit/adf6390fa34e8f7f720ec7fbbaa9f35e4a1ed383))
* **spinup:** ServiceProvisioner, ServiceSpec and the resource table (PRSR-27) ([#37](https://github.com/Einlanzerous/purser/issues/37)) ([d8822a3](https://github.com/Einlanzerous/purser/commit/d8822a35f3a93887f4d3444e0f3a1aff957aba66))
* **spinup:** the DNS record provisioner (PRSR-28) ([#44](https://github.com/Einlanzerous/purser/issues/44)) ([8a8c016](https://github.com/Einlanzerous/purser/commit/8a8c016cce0fb25468a55e151861adbc181ab494))
* **spinup:** tunnel ingress-route provisioner (PRSR-30) ([#43](https://github.com/Einlanzerous/purser/issues/43)) ([c72522b](https://github.com/Einlanzerous/purser/commit/c72522b8caf2a11f6fbb3232cac814c0e952e778))


### Bug Fixes

* **cfaccess:** an application serving a path is not that hostname's application (PRSR-41) ([#49](https://github.com/Einlanzerous/purser/issues/49)) ([0221462](https://github.com/Einlanzerous/purser/commit/0221462b406c77112ccdc842c0a06ae24aaf2bd1))

## [0.15.0](https://github.com/Einlanzerous/purser/compare/v0.14.0...v0.15.0) (2026-08-23)


### Features

* **health:** report version and sha on /healthz (PRSR-32) ([#35](https://github.com/Einlanzerous/purser/issues/35)) ([0b3ff3c](https://github.com/Einlanzerous/purser/commit/0b3ff3c65fc0915d5b7959b795277944810f6569))

## [0.14.0](https://github.com/Einlanzerous/purser/compare/v0.13.0...v0.14.0) (2026-08-15)


### Features

* **config:** carry the Cloudflare zone + tunnel ids (PRSR-11) ([#33](https://github.com/Einlanzerous/purser/issues/33)) ([3354ab9](https://github.com/Einlanzerous/purser/commit/3354ab90b99917d1047014ce9195c87d258d489b))

## [0.13.0](https://github.com/Einlanzerous/purser/compare/v0.12.0...v0.13.0) (2026-08-08)


### Features

* **offboard:** give Purser the revoke half it never had (PRSR-17) ([#29](https://github.com/Einlanzerous/purser/issues/29)) ([b1d5b22](https://github.com/Einlanzerous/purser/commit/b1d5b22e90397e3ba09336e3728ffabe03129da6))

## [0.12.0](https://github.com/Einlanzerous/purser/compare/v0.11.0...v0.12.0) (2026-08-08)


### Features

* **person:** read the roster back without psql (PRSR-24) ([#26](https://github.com/Einlanzerous/purser/issues/26)) ([2ffb7bb](https://github.com/Einlanzerous/purser/commit/2ffb7bb6bdf2dfacea0df4b8742ea40607ee9c26))

## [0.11.0](https://github.com/Einlanzerous/purser/compare/v0.10.0...v0.11.0) (2026-08-04)


### ⚠ BREAKING CHANGES

* **invite:** require an email, the key idempotency is built on (PRSR-23) ([#24](https://github.com/Einlanzerous/purser/issues/24))

### Bug Fixes

* **invite:** require an email, the key idempotency is built on (PRSR-23) ([#24](https://github.com/Einlanzerous/purser/issues/24)) ([27b4aa8](https://github.com/Einlanzerous/purser/commit/27b4aa8fa7ad3e117799f491b9781b68b4d93bf5))

## [0.10.0](https://github.com/Einlanzerous/purser/compare/v0.9.1...v0.10.0) (2026-08-02)


### ⚠ BREAKING CHANGES

* **invite:** give a not-yet-ready connector its own status (PRSR-21) ([#21](https://github.com/Einlanzerous/purser/issues/21))

### Features

* **invite:** give a not-yet-ready connector its own status (PRSR-21) ([#21](https://github.com/Einlanzerous/purser/issues/21)) ([636adc5](https://github.com/Einlanzerous/purser/commit/636adc5428840bd900801e68081741a416ff7599))


### Maintenance

* keep breaking changes on a minor bump until 1.0 ([#23](https://github.com/Einlanzerous/purser/issues/23)) ([3121ea3](https://github.com/Einlanzerous/purser/commit/3121ea35aff21a927e757772aab07d7324985f3e))

## [0.9.1](https://github.com/Einlanzerous/purser/compare/v0.9.0...v0.9.1) (2026-08-02)


### Bug Fixes

* **invite:** keep the operator's failure list out of emailed invites (PRSR-19) ([#18](https://github.com/Einlanzerous/purser/issues/18)) ([31e1ec2](https://github.com/Einlanzerous/purser/commit/31e1ec281aa1a5eb09383afe48527ae2a8c9849f))
* **invite:** stop silently renaming an existing person (PRSR-20) ([#20](https://github.com/Einlanzerous/purser/issues/20)) ([fc0d027](https://github.com/Einlanzerous/purser/commit/fc0d0274793d9bb323b48eb86882812828efc6c9))

## [0.9.0](https://github.com/Einlanzerous/purser/compare/v0.8.0...v0.9.0) (2026-08-02)


### Features

* **person:** add a person without provisioning them (PRSR-16) ([#16](https://github.com/Einlanzerous/purser/issues/16)) ([26a8d08](https://github.com/Einlanzerous/purser/commit/26a8d083b0713784e7519fee59979b5f973bfb8f))

## [0.8.0](https://github.com/Einlanzerous/purser/compare/v0.7.0...v0.8.0) (2026-07-27)


### Features

* **invite:** lead the credential block with the Access launcher (PRSR-12) ([#13](https://github.com/Einlanzerous/purser/issues/13)) ([0bd88ad](https://github.com/Einlanzerous/purser/commit/0bd88ad0a75a94edfbbd48e892674eb062b851c3))

## [0.7.0](https://github.com/Einlanzerous/purser/compare/v0.6.1...v0.7.0) (2026-07-25)


### Features

* **argosy:** reconcile via the new lookup endpoint (ARGY-163) ([#11](https://github.com/Einlanzerous/purser/issues/11)) ([7dfe78f](https://github.com/Einlanzerous/purser/commit/7dfe78f32cf918738b613245d19bae4111bcff6f))

## [0.6.1](https://github.com/Einlanzerous/purser/compare/v0.6.0...v0.6.1) (2026-07-25)


### Bug Fixes

* **lyceum:** implement Reconcile — the lookup endpoint already exists (SERV-54) ([#9](https://github.com/Einlanzerous/purser/issues/9)) ([b68acfc](https://github.com/Einlanzerous/purser/commit/b68acfca73aecb15013fd22951c5248950c6c3b5))

## [0.6.0](https://github.com/Einlanzerous/purser/compare/v0.5.0...v0.6.0) (2026-07-25)


### Features

* **audit:** reconcile / record-only mode (SERV-54) ([#7](https://github.com/Einlanzerous/purser/issues/7)) ([57f1d4d](https://github.com/Einlanzerous/purser/commit/57f1d4dc05e99226ee65c9338b8ed5891630f7c2))

## [0.5.0](https://github.com/Einlanzerous/purser/compare/v0.4.0...v0.5.0) (2026-07-25)


### Features

* **invite:** onboarding bundles — named service sets (SERV-47) ([#5](https://github.com/Einlanzerous/purser/issues/5)) ([002a770](https://github.com/Einlanzerous/purser/commit/002a77097c6e6e533358a52f794a4c09259cf628))

## [0.4.0](https://github.com/Einlanzerous/purser/compare/v0.3.0...v0.4.0) (2026-07-24)


### Features

* **argosy:** activate the Argosy connector (SERV-50) ([0b80f2e](https://github.com/Einlanzerous/purser/commit/0b80f2e32df7affc5a3efd44722c56d2f1bb7cd2))
* **argosy:** activate the Argosy connector (SERV-50) ([77e7f3d](https://github.com/Einlanzerous/purser/commit/77e7f3d126431159768d6056ecc37d7f8f69790d))

## [0.3.0](https://github.com/Einlanzerous/purser/compare/v0.2.0...v0.3.0) (2026-07-15)


### Features

* **credential:** per-service emojis + lead with SSO, token as fallback ([c8ababc](https://github.com/Einlanzerous/purser/commit/c8ababc7b2dcc89df94b719b9cddbc6fd0fc5488))
* role/scopes/memberships at invite, DB retry, Lyceum connector ([cf91c49](https://github.com/Einlanzerous/purser/commit/cf91c49cfc5074ff1c654ba439545e324973a6f6))

## [0.2.0](https://github.com/Einlanzerous/purser/compare/v0.1.0...v0.2.0) (2026-07-14)


### Features

* initial Purser cross-service provisioning & invite service (IDEA-14) ([e9ca2f0](https://github.com/Einlanzerous/purser/commit/e9ca2f0fce77408898246035bfc8ce269af8a219))


### Bug Fixes

* harden connectors, delivery, and API from audit findings ([70bb6fd](https://github.com/Einlanzerous/purser/commit/70bb6fdaeb29167549a84e679b0eefe04705a0ce))

## Changelog
