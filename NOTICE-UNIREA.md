# Unirea Cloud changes

This fork of BackupZit remains licensed under the GNU Affero General Public
License, version 3 (AGPLv3). Original copyright and license notices are retained.

Changes made by Unirea Cloud, starting 7 November 2026:

- Parse console templates on first use to reduce idle memory; preserve all
  supported languages and documentation search.
- Add limited-use enrollment tokens, atomic agent limits and enrollment
  provenance, with public API endpoints and concurrency tests.
- Keep API audit labels separate from user identities used for approval.
- Share storage, job and file-restore mutations between the UI and public API,
  including encryption, audit, command permissions and four-eyes approvals.
- Extend the agent protocol with per-job Windows VSS and disk selections, and
  add yearly retention alongside the existing retention rules.
- Expose snapshot file listing, streaming file/ZIP downloads and agent
  restoration; add stable run cursors with filters applied before the limit.
- Add one-shot CLI API-token provisioning and initial password files, with
  token-only stdout and audit entries that exclude the token secret.
- Document all public routes in an embedded OpenAPI 3.1 contract and update
  the English API guide, with contract-to-route and response-shape tests.
- Negotiate extended job support explicitly through agent capabilities; escape
  S3 prefixes and display yearly-only retention safely in the console.
- Expose an authenticated agent installer catalog and allowlisted public
  downloads with real, cached SHA-256 checksums and safe directory resolution.
- Add persistent agent display names and API removal sharing UI approvals and
  audit, retaining backups and history even without registered repositories.
- Verify stable agent/run IDs and readable runs after job removal, and document
  unattended Windows MSI and Linux DEB/RPM installation with checksum checks.
