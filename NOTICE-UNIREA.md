# Unirea Cloud changes

This fork of BackupZit remains licensed under the GNU Affero General Public
License, version 3 (AGPLv3). Original copyright and license notices are retained.

Changes made by Unirea Cloud, starting 7 November 2026:

- Parse console templates on first use to reduce idle memory; preserve all
  supported languages and documentation search.
- Add limited-use enrollment tokens, atomic agent limits and enrollment
  provenance, with public API endpoints and concurrency tests.
- Keep API audit labels separate from user identities used for approval.
