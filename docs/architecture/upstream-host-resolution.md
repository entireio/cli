# Upstream Host & Login Resolution

The CLI stores one current login in the configured credential backend (the OS
keychain by default). A login contains its issuer/login-server URL, account
handle, and the credential slots holding its access and refresh tokens.

`entire login` replaces the current login. `entire logout` removes it.

## Services

| Role | Hit by | Login-server discovery |
|---|---|---|
| Login server / control plane | `org`, `repo`, `project`, `grant`, `auth` | The current login's issuer |
| Git cluster | clone/push | `/.well-known/entire-cluster.json` |
| Web/data API | `activity`, `search`, `trail`, `dispatch` | `/.well-known/entire-api.json` |

## Resolution

### Control plane

The CLI dials the current login's issuer and uses its refreshable login JWT. If
there is no current login, the command asks the user to run `entire login`.

### Git clusters

The cluster advertises the login servers it trusts. The CLI verifies that the
current login was issued by one of them, then exchanges credentials as needed.
An incompatible login is never sent to the cluster; the user is asked to log in
again.

### Web/data API

The API advertises trusted issuers. The CLI verifies the current login against
that list and exchanges its login JWT for a token whose audience is the API
origin. Discovery is cached with a 24-hour TTL and stale fallback.

`ENTIRE_TOKEN` remains an explicit, process-local override for supported command
paths and does not alter the stored login.

## Storage and migration

Login metadata is stored under one fixed logical credential entry per Entire
config directory. Tokens remain in the same credential backend. On first use,
older `contexts.json` installations migrate only their previously active login;
the file and credentials for other saved logins are removed.
