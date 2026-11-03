# RHIS Remote

Self-hosted remote support for Windows: remote control with unattended access, a one-off quick support client, and a modular server that runs on a single Debian 13 machine and scales out to multiple servers.

> Status: early development. See the [sketches](https://claude.ai/artifact/EGUbiA7E5estczA15PKzjA) for the planned architecture and screens.

## Branches

| Branch | Channel | Purpose |
| --- | --- | --- |
| `main` | Production | Stable releases |
| `test` | Test | Pre-releases for testing; feature work is merged here first |

## Branding

The `branding/` folder holds the RHIS house style (logos, colors, fonts) used by the web portal and clients. Product name, logo and colors are configurable so other brands can ship their own build.

## License

Copyright (C) 2026 RHIS.

This project is licensed under the [GNU Affero General Public License v3.0](LICENSE) (AGPL-3.0-only). If you run a modified version as a network service, you must make your source code available to its users.
