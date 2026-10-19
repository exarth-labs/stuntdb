# Security policy

stuntdb works with production databases, so we take security reports seriously.

## Supported versions

stuntdb has no stable release yet. Security fixes go into the latest release only.

## Reporting a vulnerability

**Please don't open a public issue.** Report the problem privately through GitHub: open the repository's **Security** tab and choose **Report a vulnerability**, or go directly to https://github.com/exarth-labs/stuntdb/security/advisories/new.

Please include:

- what you found and what impact it has
- steps to reproduce (a minimal example schema helps)
- your stuntdb version (`stuntdb version`) and operating system

## What counts as a security issue

As well as the usual kinds, such as code execution or exposed credentials, this one counts: **stuntdb leaving real personal data in its output, logs or error messages.** Preventing that is the whole purpose of the tool.

## What happens next

- We aim to acknowledge your report within 5 working days.
- We'll keep you updated while we work on a fix.
- If you wish, we'll credit you in the release notes.
