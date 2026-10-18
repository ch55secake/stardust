# Release artifact verification

Stardust publishes four archives (Linux and macOS, amd64 and arm64), a SHA-256
`checksums.txt`, and a detached `checksums.txt.sig` signature. GoReleaser signs
the checksum manifest with RSA/SHA-256 using the private key in the GitHub
Actions secret `STARDUST_RELEASE_SIGNING_KEY`. The release workflow fails before
creating a tag or release if that secret is absent or invalid.

The matching RSA public key is embedded in `install.sh`; it is not downloaded
alongside the release. The installer downloads the checksum manifest and
signature over HTTPS, verifies the signature against its embedded trust roots,
and only then reads the manifest or downloads an archive. It subsequently
checks the archive digest before extracting or replacing the installed binary.
Missing or invalid signatures, unsupported verification tools, and checksum
mismatches fail closed. Linux and macOS installers both require `openssl`.

## Initial key provisioning

The repository's Actions secret is provisioned with the current dedicated
3072-bit RSA private key. For a new repository or a planned key replacement,
generate and store a key offline; never commit, print in CI logs, or otherwise
publish the private key:

```sh
umask 077
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out stardust-release-private.pem
openssl pkey -in stardust-release-private.pem -pubout -out stardust-release-public.pem
gh secret set STARDUST_RELEASE_SIGNING_KEY --repo ch55secake/stardust < stardust-release-private.pem
```

The Actions secret contains the PEM private key. Keep a separate, encrypted
offline backup under release-maintainer control. The corresponding public key
must be added to the `trusted_release_public_keys` array in `install.sh`.

## Key rotation

1. Generate a new keypair offline and add the new public key to
   `trusted_release_public_keys` in `install.sh`. Keep the existing public key
   in the array. Merge this change before changing the Actions secret.
2. Replace `STARDUST_RELEASE_SIGNING_KEY` with the new private key. Future
   releases are signed by the new key; installers containing the updated script
   accept signatures from both the old and new keys, preserving verification for
   older pinned releases. Users with a copied older installer must fetch the
   current installer before upgrading to a release signed only by the new key.
3. After releases signed by the old key are no longer supported, remove that
   old public key in a later change. Do not remove a trust root while users are
   expected to install releases signed with it.

The installer script is the trust bootstrap: obtain it from the official
repository over HTTPS (or verify/pin the script through an independently trusted
channel in higher-assurance environments). A compromised release asset or
release-upload credential alone cannot produce a valid signature without the
separate signing key. Protect repository write access, release workflow changes,
and the Actions secret as part of the same release security boundary.

Automated installer tests generate ephemeral keys and cover successful
verification, altered/missing signatures, altered checksum metadata, and
tampered archives. They never use or expose the production signing key.
