# ZFS module

This module is a fork of the go-zfs module located at:

- https://github.com/mistifyio/go-zfs

Because I needed many changes to support encrypted ZFS support and the module has not seen much recent development I decided to fork the module.

## Testing

To test this module run this on a system with ZFS tools enabled:

- `sudo go test -v ./`

Sudo permissions are required to run `zpool` commands unfortunately. The tests create a test zpool using some files in `/tmp`.
