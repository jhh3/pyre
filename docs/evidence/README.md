# Evidence

Redacted, reproducible evidence for gates and acceptance cases. Nothing here may contain tokens, OAuth material, account emails, private transcripts, vault content, hostnames or home paths.

Real-host evidence (the runtime and accounts spike reports, Hive inventory, the acceptance matrix and integrated fleet runs, install/upgrade rehearsals on real machines) is kept in a private evidence store, not in this repository. Docs that cite a report by name (for example "accounts report", `ROTA-CHECKOUT.md`, `DOORBELL.md`) refer to that store. Only synthetic, reproducible output belongs here.

| Directory | Owner | Contents |
| --- | --- | --- |
| `android/` | WP16 | Emulator verification of the Android client |
| `release/` | WP21 | Release-candidate evidence summary |
| `review/` | integration owner | State-of-the-art technology review and upgrade plan |
| `security/` | security fixes (W-secrets) | Credential exposure audits and their real-host verification |

Each record states commit, platform, fixture/profile, commands, observed outcome and log path. An unrun case is marked unrun. Mock-backed results are labelled mock and never count as runtime evidence.
