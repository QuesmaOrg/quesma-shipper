# Trademark policy

Quesma, Quesma Shipper, and the Quesma logo are trademarks of Quesma Inc.

This policy explains how you may use those marks. It does not limit what you may do with the code.
The code is licensed under the [Apache License 2.0](LICENSE). Section 6 of that license grants no
trademark rights, so the license and this policy operate independently.

## Why this policy exists

The code is open. The brand is not. A product name tells users who stands behind a binary: who
built it, who signed it, who serves its configuration, and who answers a security report. A fork
that keeps the name makes those promises on our behalf. We cannot keep them for code we did not
build, so a modified version must carry a different name.

This is the same model as Firefox and Iceweasel, Chrome and Chromium, VS Code and VSCodium, and
many other open source projects.

## What you may do without permission

- Use, modify, fork, and redistribute the code under the Apache License 2.0, for any purpose the
  license allows.
- State truthfully that your software is based on, derived from, or compatible with Quesma
  Shipper. Examples: "based on Quesma Shipper", "a fork of Quesma Shipper", "implements the
  Quesma Shipper protocol".
- Refer to Quesma and Quesma Shipper by name in documentation, articles, comparisons, and
  discussions.
- Use the wire-protocol identifiers defined in [PROTOCOL.md](PROTOCOL.md), such as header names
  and signing prefixes. They are part of the protocol, not the brand, and any implementation may
  use them.
- Redistribute unmodified official releases under their original name.

## What you must do when you distribute a modified version

A modified version is any build that is not an unmodified official release from Quesma Inc.

- Do not name the binary, process, package, container image, service, or repository
  `quesma-shipper`, `Quesma Shipper`, or any name that contains `Quesma`.
- Do not use the Quesma logo.
- Remove `Quesma` from the command-line name, help text, version string, and other user-facing
  output. If you keep any Quesma string, state clearly and prominently that the software is an
  unofficial fork and is not endorsed or supported by Quesma Inc. Put this statement in the first
  screen of `--help`, in the README, and in the package description.
- Do not state or imply that Quesma Inc. endorses, certifies, or supports your version.
- Do not register domain names, social media accounts, or package registry names that contain
  `Quesma`.

## What this policy does not do

It does not stop anyone from using the code, including for purposes we would not choose. It stops
them from doing so under our name. Users who see the Quesma name should be able to trust that
Quesma Inc. built what they are running.

## Governance

We intend to move Quesma Shipper and this protocol to vendor-neutral governance, such as a Linux
Foundation project, under a vendor-neutral name. When that happens, the foundation's trademark
policy replaces this one. Contributions and adoption bring that day closer.

## Questions and permission

For any use not covered here, or to ask for permission, email `contact@quesma.com` with the
subject `Trademark: shipper-protocol`.
