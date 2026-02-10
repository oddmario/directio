# Linux Direct IO Writer

Direct IO writer using O_DIRECT

## Why the fork?
The original project is really amazing, However in certain cases it can cause dome data to be written on the pagecache when `Flush()` gets called.

The original project's `Flush()` function disables O_DIRECT, writes the remaining data then re-enables O_DIRECT. I understand why it's made like this (because maybe the remaining data don't perfectly align into a disk page)

My fork aims to solve this by minimizing the data written to the pagecache as much as possible. The `Flush()` function was replaced with the `Close()` one.

With my fork, you must call `Close()` after writing everything to ensure that everything gets written to the disk. Note that with the original project, you also must call `Flush()` after writing everything.

> [!WARNING]
> `/tmp/` in modern systems doesn't supoprt Direct I/O, for tests use `/var/tmp` instead.

Example:

```go
package main

import (
    "io"
    "log"
    "net/http"
    "os"
    "syscall"

    "github.com/oddmario/directio"
)

func main() {
    // Open file with O_DIRECT
    flags := os.O_WRONLY | os.O_EXCL | os.O_CREATE | syscall.O_DIRECT
    f, err := os.OpenFile("/var/tmp/mini.iso", flags, 0644)
    if err != nil {
        log.Fatal(err)
    }
    defer f.Close()

    // Use directio writer
    dio, err := directio.New(f)
    if err != nil {
        log.Fatal(err)
    }
    defer dio.Close()

    // Downloading iso image
    resp, err := http.Get("http://archive.ubuntu.com/ubuntu/dists/bionic/main/installer-amd64/current/images/netboot/mini.iso")
    if err != nil {
        log.Fatal(err)
    }
    defer resp.Body.Close()

    // Write the body to file
    _, err = io.Copy(dio, resp.Body)
}

```

Check that dio bypass linux pagecache using `vmtouch`:

```bash
$ vmtouch /var/tmp/mini.iso
           Files: 1
     Directories: 0
  Resident Pages: 1/16384  4K/64M  0.0061%
         Elapsed: 0.000356 seconds
```

or using my `https://github.com/brk0v/cpager` to check per cgroup pagecache usage:

```bash
$ sudo ~/go/bin/cpager /var/tmp/mini.iso
         Files: 1
   Directories: 0
Resident Pages: 1/16385 4K/64M 0.0%

 cgmem inode    percent       pages        path
           -     100.0%       16384        not charged
        2187       0.0%           1        /sys/fs/cgroup/memory/user.slice/user-1000.slice/session-3.scope
```
