# Changelog

## [0.9.0](https://github.com/zeroroot-ai/connectors/compare/v0.8.2...v0.9.0) (2026-11-24)


### Features

* **fixloop:** close_merge_request takes an optional comment ([#44](https://github.com/zeroroot-ai/connectors/issues/44)) ([a0317ff](https://github.com/zeroroot-ai/connectors/commit/a0317ff64c5fb0b22db55cbb3733f7ab257f088d))


### Bug Fixes

* **release:** build each release image once and list the version tag digest ([#42](https://github.com/zeroroot-ai/connectors/issues/42)) ([05adcf1](https://github.com/zeroroot-ai/connectors/commit/05adcf129e9febaf5afde9b11adb247b8c82e2ce))

## [0.8.2](https://github.com/zeroroot-ai/connectors/compare/v0.8.1...v0.8.2) (2026-11-23)


### Bug Fixes

* **fixloop:** name the open request and drop a false retry hint in open errors ([#39](https://github.com/zeroroot-ai/connectors/issues/39)) ([2453925](https://github.com/zeroroot-ai/connectors/commit/245392563fd595624242c4b5eeb06a648619b5fd))

## [0.8.1](https://github.com/zeroroot-ai/connectors/compare/v0.8.0...v0.8.1) (2026-11-23)


### Bug Fixes

* **fixloop:** find the open pull request beside a closed one, and keep a write visible ([#37](https://github.com/zeroroot-ai/connectors/issues/37)) ([48286ce](https://github.com/zeroroot-ai/connectors/commit/48286ced0f40660ecd2e9577ebbf9034cc306b55))

## [0.8.0](https://github.com/zeroroot-ai/connectors/compare/v0.7.0...v0.8.0) (2026-11-23)


### Features

* **kit:** put the class of a tool error in structuredContent ([#35](https://github.com/zeroroot-ai/connectors/issues/35)) ([cf0df5b](https://github.com/zeroroot-ai/connectors/commit/cf0df5b36e3d71ab367ebdbd7dca8be57165e7de))

## [0.7.0](https://github.com/zeroroot-ai/connectors/compare/v0.6.0...v0.7.0) (2026-11-23)


### Features

* **fixloop:** add list_tree to github-fixloop and gitlab-fixloop ([#31](https://github.com/zeroroot-ai/connectors/issues/31)) ([0fbdf11](https://github.com/zeroroot-ai/connectors/commit/0fbdf11b082f8b12519c370ba30b9d21abd59655))


### Bug Fixes

* **github-fixloop:** keep the executable bit when open_merge_request edits a file ([#32](https://github.com/zeroroot-ai/connectors/issues/32)) ([76863d6](https://github.com/zeroroot-ai/connectors/commit/76863d6bb9b94307a5c3d8c996fa012074fec8cb))
* **gitlab-fixloop:** bind the list_tree cursor to the project, the ref and the path ([#34](https://github.com/zeroroot-ai/connectors/issues/34)) ([7853faa](https://github.com/zeroroot-ai/connectors/commit/7853faa5b975a7ed02ec59f8c18020c6e1d3994d))

## [0.6.0](https://github.com/zeroroot-ai/connectors/compare/v0.5.0...v0.6.0) (2026-11-23)


### Features

* **fixloop:** add list_dependency_alerts to github-fixloop and gitlab-fixloop ([#29](https://github.com/zeroroot-ai/connectors/issues/29)) ([0a169fe](https://github.com/zeroroot-ai/connectors/commit/0a169fe119fd0c7ea37f2e13bd1501dd973fc8a2))

## [0.5.0](https://github.com/zeroroot-ai/connectors/compare/v0.4.0...v0.5.0) (2026-11-23)


### Features

* **mock-forge:** add a stateful GitHub and Prometheus mock test image ([#27](https://github.com/zeroroot-ai/connectors/issues/27)) ([0f911b6](https://github.com/zeroroot-ai/connectors/commit/0f911b64f5c681881110ac960b668fc2055426a1))

## [0.4.0](https://github.com/zeroroot-ai/connectors/compare/v0.3.0...v0.4.0) (2026-11-22)


### Features

* **ticket:** search_tickets finds a ticket by dedup_key ([#25](https://github.com/zeroroot-ai/connectors/issues/25)) ([1b3a881](https://github.com/zeroroot-ai/connectors/commit/1b3a88167bfa1b2a61e5db22e285298a69f8f444))

## [0.3.0](https://github.com/zeroroot-ai/connectors/compare/v0.2.0...v0.3.0) (2026-11-22)


### Features

* **cortex-xdr:** add the server for incidents, alerts, endpoints and isolate ([#20](https://github.com/zeroroot-ai/connectors/issues/20)) ([05ee061](https://github.com/zeroroot-ai/connectors/commit/05ee061cdc4b4128794c0beaf04ac16d3c05db93))
* **fixloop:** add open_merge_request, close_merge_request and read_file to the git-host servers ([#11](https://github.com/zeroroot-ai/connectors/issues/11)) ([93029bb](https://github.com/zeroroot-ai/connectors/commit/93029bb7ccf8678fcb311a9a6e6e430a025b2bd6))
* **jira:** add the issue server on the Jira Cloud REST API v3 ([#15](https://github.com/zeroroot-ai/connectors/issues/15)) ([3393fa5](https://github.com/zeroroot-ai/connectors/commit/3393fa586ea5292704535259dd7fd3de7515278e))
* **microsoft-intune:** add the patch server on the Graph beta deployment service ([#10](https://github.com/zeroroot-ai/connectors/issues/10)) ([08b19e9](https://github.com/zeroroot-ai/connectors/commit/08b19e95716ea8b2ef8cdf65a3a4b33cd81cd4c3))
* **okta:** add read tools for log events, users and sessions ([#8](https://github.com/zeroroot-ai/connectors/issues/8)) ([e011b62](https://github.com/zeroroot-ai/connectors/commit/e011b626adb2fd3ba6252d19932886b3b20b1546))
* **pagerduty:** add the paging server on the Events API v2 and the REST API v2 ([#16](https://github.com/zeroroot-ai/connectors/issues/16)) ([9612511](https://github.com/zeroroot-ai/connectors/commit/961251144a4a25dcaf3fb242e5894bcbad71230f))
* **rapid7-insightidr:** add the read server for investigations and alerts ([#19](https://github.com/zeroroot-ai/connectors/issues/19)) ([8ab52bb](https://github.com/zeroroot-ai/connectors/commit/8ab52bb8fa7c99bd407265ced514f9432699fff1))
* **rapid7-insightvm:** add the read server on the Console API v3 ([#18](https://github.com/zeroroot-ai/connectors/issues/18)) ([b30a53c](https://github.com/zeroroot-ai/connectors/commit/b30a53c7bd5939317ae4f9ba180eead668bc1a69))
* **sentinelone:** add read tools for threats and agent state ([#23](https://github.com/zeroroot-ai/connectors/issues/23)) ([a6d07b5](https://github.com/zeroroot-ai/connectors/commit/a6d07b52456270a71bdeec6a41f3c6bfe400c088))
* **servicenow:** add the incident server on the Table API ([#14](https://github.com/zeroroot-ai/connectors/issues/14)) ([b32816a](https://github.com/zeroroot-ai/connectors/commit/b32816a9a1a7243b73d9fa898c518f468d1e7517))
* **ticket:** add the shared tool contract of the ticket servers ([#13](https://github.com/zeroroot-ai/connectors/issues/13)) ([49668f7](https://github.com/zeroroot-ai/connectors/commit/49668f7a6b055544cbe7d6274aae21ec3c6c19de))
* **trellix:** add the read server for alerts and hosts ([#17](https://github.com/zeroroot-ai/connectors/issues/17)) ([417cb04](https://github.com/zeroroot-ai/connectors/commit/417cb04055d4d41fae6bedd257308dd5b051f02d))


### Bug Fixes

* **github-fixloop:** pin the changed files to the head and close the review minors ([#24](https://github.com/zeroroot-ai/connectors/issues/24)) ([310f8f2](https://github.com/zeroroot-ai/connectors/commit/310f8f2050729eb343e280cf9f9189c4d6486635))
* **kit:** wait for X-Rate-Limit-Reset on a 429 ([#12](https://github.com/zeroroot-ai/connectors/issues/12)) ([54391d2](https://github.com/zeroroot-ai/connectors/commit/54391d2eb7c37f9b8a5a7260230df895e37e038e))

## [0.2.0](https://github.com/zeroroot-ai/connectors/compare/v0.1.0...v0.2.0) (2026-11-21)


### Features

* **fixloop:** return the source branch and the creation time of a merge request ([#6](https://github.com/zeroroot-ai/connectors/issues/6)) ([01a4286](https://github.com/zeroroot-ai/connectors/commit/01a4286e441ab0336ef5bb0fe5e832152299413d))

## 0.1.0 (2026-11-20)


### Features

* add the first connector servers and the fix loop servers ([#1](https://github.com/zeroroot-ai/connectors/issues/1)) ([a57daab](https://github.com/zeroroot-ai/connectors/commit/a57daab8dc525f3d33bae1c5babd9997be654739))


### Bug Fixes

* **release:** start the first release at 0.1.0 ([#3](https://github.com/zeroroot-ai/connectors/issues/3)) ([60cc12f](https://github.com/zeroroot-ai/connectors/commit/60cc12f582a33613491893ae9d1812710f5c58c1))
