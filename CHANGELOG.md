# Changelog

## 0.1.0 (2026-10-15)


### Features

* **rsync:** --backup, --backup-dir, --suffix ([b88f0ac](https://github.com/parallelworks/xfer/commit/b88f0ac78d260e8bc6ae1288321044875392ac09))
* **rsync:** --delay-updates ([79b81e4](https://github.com/parallelworks/xfer/commit/79b81e4a0f45ceac91ec56fc013ce05dbe404ce9))
* **rsync:** --files-from and --from0, with list forwarding both ways ([5272fc9](https://github.com/parallelworks/xfer/commit/5272fc97641472bdf33763cb786a288c0dc8d7ac))
* **rsync:** --link-dest, --copy-dest, --compare-dest ([495d207](https://github.com/parallelworks/xfer/commit/495d207799f0e9e11434e2cd1990b2a5bcce2ee1))
* **rsync:** --skip-compress with rsync's default suffix list (zlib, zlibx) ([79dae8e](https://github.com/parallelworks/xfer/commit/79dae8eaaa79ea617f9b308159e1a7ceaa38db56))
* **rsync:** --timeout with keep-alives, --contimeout ([04b584e](https://github.com/parallelworks/xfer/commit/04b584e4c740215ae866e860ba2c962f3fe77f5b))
* **rsync:** -K, --super/--no-super, -8 and rsync's name escaping; local transfers itemize as received ([06d008a](https://github.com/parallelworks/xfer/commit/06d008a87b84240d11dfdb32003701c294f7da8c))
* **rsync:** -m/--prune-empty-dirs ([f3ced81](https://github.com/parallelworks/xfer/commit/f3ced81322dad631624be0379cb02b87b85c87e7))
* **rsync:** -S/--sparse writes holes ([3cca888](https://github.com/parallelworks/xfer/commit/3cca88851d4649419d09a5b35f0fba3bc556428d))
* **rsync:** an event stream for embedders, options that were ignored, no global state ([c8fe3b2](https://github.com/parallelworks/xfer/commit/c8fe3b24b22f6619b8d097c7252a2ed0e0dd39bc))
* **rsync:** GNU-compatible rsync engine, CLI and transports ([3da25e7](https://github.com/parallelworks/xfer/commit/3da25e73fde476297b5372dde7b20ec0519b4cdd))
* **rsync:** honour per-directory merge files in the receiver's delete scan ([1152fd6](https://github.com/parallelworks/xfer/commit/1152fd66e97e1cf6379ac9266b13ce38dba82745))
* **rsync:** incremental recursion ([0a05da1](https://github.com/parallelworks/xfer/commit/0a05da14e9dae4265a816b99909bbc92cb7b52a8))
* **rsync:** protocol 33 and rsync's byte accounting ([5ed9a04](https://github.com/parallelworks/xfer/commit/5ed9a049bb36157b545572b4839bdb78ecea8602))
* **rsync:** rsync's --info/--debug levels and verbose output, with golden tests ([6e08612](https://github.com/parallelworks/xfer/commit/6e08612b45b02525ccaa72b17d53c0268b7871e1))


### Bug fixes

* **filter:** empty-pattern rules; add a parse/match fuzzer ([15a1f82](https://github.com/parallelworks/xfer/commit/15a1f823b4eebf759bdb25de65f1ca68638b3e3d))
* **rsync:** --help names the protocol versions spoken, 27 to 33 ([0cabee0](https://github.com/parallelworks/xfer/commit/0cabee09b7f775e94f2920512e0fdb4e556af31a))
* **rsync:** bound --contimeout like --timeout; ignore worktrees ([7ee23f7](https://github.com/parallelworks/xfer/commit/7ee23f7011c2478a974e019f5270d225a80c60d0))
* **rsync:** error messages in rsync's form ([1a5a441](https://github.com/parallelworks/xfer/commit/1a5a441c8581e2eab76f31b6aab714cc49c37bb2))
* **rsync:** exact sizes beyond 2^53 ([df47dd2](https://github.com/parallelworks/xfer/commit/df47dd2aef834792d303540123705150c80c3779))
* **rsync:** judge unsafe links by their name in the transfer, not on disk ([1f04474](https://github.com/parallelworks/xfer/commit/1f04474bce904871e337ddf1871ed3a0365ebf31))
* **rsync:** quote names in error messages as rsync does ([df6adb7](https://github.com/parallelworks/xfer/commit/df6adb7bd60f02063b691d72ec2c7cc4289ce83b))
* **rsync:** server operands and duration overflow; add an args fuzzer ([062d8a5](https://github.com/parallelworks/xfer/commit/062d8a50b0e2778d83d16ea7ae5a07dcb2e8c352))
* **rsync:** staticcheck findings ([f611080](https://github.com/parallelworks/xfer/commit/f61108057e956dedf7ef6a983ee4f17217581ce0))
* **rsync:** two fixes from the argument fuzzer ([ee33863](https://github.com/parallelworks/xfer/commit/ee33863cdd42f1c104ad40c6017c48e1dbabf07f))


### Performance

* **delta:** no hash-hit tag set for files without blocks; pool the rest ([c5793cd](https://github.com/parallelworks/xfer/commit/c5793cd3c006183cef5092ed05af59ad40af72bb))
* directory handle cache, AVX2 rolling checksum, bounded delta buffer ([6f3fea6](https://github.com/parallelworks/xfer/commit/6f3fea63038a7ef8769d792c3262b9fc3d450b66))
* fewer syscalls per file ([64a784b](https://github.com/parallelworks/xfer/commit/64a784b0cf7a1dfda907635ed50eeefe753bd61f))
* **rsync:** 64 KB I/O buffers ([164ea6e](https://github.com/parallelworks/xfer/commit/164ea6e469723a5c754c2887ced0186a919e6ff8))
* **token:** a 2 MB zstd window, as zstd's default level uses ([784df5b](https://github.com/parallelworks/xfer/commit/784df5bfc1698bcc215bd457ba27422659e6c29b))
* **token:** pipeline zstd match finding and entropy coding ([9aba7ef](https://github.com/parallelworks/xfer/commit/9aba7eff890eacb81fddcd4c936de9fb7370a644))
* **xfer:** collect garbage at GOGC=25 unless GOGC is set ([531f141](https://github.com/parallelworks/xfer/commit/531f141777a8bcde1536356314374cb343ce4956))
