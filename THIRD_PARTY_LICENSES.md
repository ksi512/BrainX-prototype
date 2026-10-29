# Third-Party Licenses

BrainX itself is released under the MIT License (see [LICENSE](LICENSE)).

The following modules are linked into the `brainx` binary. They keep their own
licenses; they are not relicensed under MIT. All linked modules are permissive
(MIT, BSD-3-Clause, or public domain), and no copyleft-licensed module is
linked into the binary.

| Module | Version | License |
|---|---|---|
| github.com/dustin/go-humanize | v1.0.1 | MIT |
| github.com/google/uuid | v1.6.0 | BSD-3-Clause |
| github.com/ledongthuc/pdf | v0.0.0-20261020125853-5e46805f718e | BSD-3-Clause |
| github.com/mattn/go-isatty | v0.0.24 | MIT |
| github.com/ncruces/go-strftime | v1.0.0 | MIT |
| github.com/remyoudompheng/bigfft | v0.0.0-20230129092748-24d4a6f8daec | BSD-3-Clause |
| golang.org/x/sys | v0.48.0 | BSD-3-Clause |
| modernc.org/libc | v1.77.1 | BSD-3-Clause (includes third-party notices) |
| modernc.org/mathutil | v1.7.1 | BSD-3-Clause |
| modernc.org/memory | v1.12.1 | BSD-3-Clause (includes Go/MIT/MMAP notices) |
| modernc.org/sqlite | v1.60.1 | BSD-3-Clause (includes SQLite public domain and sqlite-vec MIT notices) |

Full license texts are in [`licenses/`](licenses/), one directory per module.
`modernc.org/sqlite` and `modernc.org/libc` also ship their upstream
`LICENSE-3RD-PARTY.md` files; those are included verbatim.

If you redistribute a binary, keep this file and the `licenses/` directory
next to the binary.
