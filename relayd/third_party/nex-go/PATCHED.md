# Patched nex-go v1.0.16 (relayd's NEX auth servers)

Upstream `github.com/PretendoNetwork/nex-go@v1.0.16` with:
- RevivetendoVendorPatches `patches/authentication` (signature before decipher, cipher lock,
  sequence ID and encryption in one step).
- The resend fix from `scripts/patch_nexgo_resend.py`: a reliable data packet is deciphered at
  most once and in order. A resent copy used to move the RC4 keystream ahead of the client's
  and break the session.

Used via `replace` in relayd/go.mod.
