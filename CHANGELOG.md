# Changelog

## [0.3.1](https://github.com/sergelogvinov/hybrid-csi-plugin/compare/v0.3.0...v0.3.1) (2025-12-16)


### Bug Fixes

* disable clean up old images ([95706ea](https://github.com/sergelogvinov/hybrid-csi-plugin/commit/95706ea69d5d8f71e6431947088cc629ba92d5e4))

## [0.3.0](https://github.com/sergelogvinov/hybrid-csi-plugin/compare/v0.2.1...v0.3.0) (2025-09-02)


### Features

* remove alpha status ([6b7b6a1](https://github.com/sergelogvinov/hybrid-csi-plugin/commit/6b7b6a1d4c98f497a1834aed3c8306728497f9cc))

## [0.2.1](https://github.com/sergelogvinov/hybrid-csi-plugin/compare/v0.2.0...v0.2.1) (2025-06-28)


### Bug Fixes

* clean old images ([9a6ef50](https://github.com/sergelogvinov/hybrid-csi-plugin/commit/9a6ef5059cfb0e44c4d1dfd6bed7e2e3c7f9857e))

## [0.2.0](https://github.com/sergelogvinov/hybrid-csi-plugin/compare/v0.1.0...v0.2.0) (2025-04-01)


### Features

* **chart:** add labels for controller pod ([4b64c88](https://github.com/sergelogvinov/hybrid-csi-plugin/commit/4b64c88408d153c9833b6ef121c6861a9bdcd2ce))


### Bug Fixes

* csidrivers permission ([2978cab](https://github.com/sergelogvinov/hybrid-csi-plugin/commit/2978cab121d46d4c800a49f934cc3e155c68778b))
* gh-action cleanup ([cbb73fb](https://github.com/sergelogvinov/hybrid-csi-plugin/commit/cbb73fb1381b35f6f3052cda196178a9b2e069ce))
* helm chart image ([41b6995](https://github.com/sergelogvinov/hybrid-csi-plugin/commit/41b69954fd31a512bcf138e6ad00193509164edb))
* helm chart metrics ([f2dcce3](https://github.com/sergelogvinov/hybrid-csi-plugin/commit/f2dcce31b3183f184ee0447ce185aec0bb757f6a))
* **main:** avoid race conditions in releasePV and bondPVC functions ([2205382](https://github.com/sergelogvinov/hybrid-csi-plugin/commit/22053820eeb0f0626cf35539d1576d2a4050b1d5)), closes [#25](https://github.com/sergelogvinov/hybrid-csi-plugin/issues/25)

## [0.1.0](https://github.com/sergelogvinov/hybrid-csi-plugin/compare/v0.0.1...v0.1.0) (2025-01-10)


### Features

* add storage class allowedTopologies ([4ba76b9](https://github.com/sergelogvinov/hybrid-csi-plugin/commit/4ba76b986490ea6a07740cf80dac027d35364b61))
* allocate pv methods ([63b453c](https://github.com/sergelogvinov/hybrid-csi-plugin/commit/63b453c22c5ccd380ae272406db33d1adce582c2))
* basic go metrics ([f13f2c9](https://github.com/sergelogvinov/hybrid-csi-plugin/commit/f13f2c9085eaea541e81d98a45dd23a95171eec1))
* hybrid plugin ([d789455](https://github.com/sergelogvinov/hybrid-csi-plugin/commit/d789455be4cf41399e1b7724681816c13f859657))
* init project ([d954dca](https://github.com/sergelogvinov/hybrid-csi-plugin/commit/d954dcaa6a314c64fcac1d770874bddd233ec12b))
* storage classes ([d313460](https://github.com/sergelogvinov/hybrid-csi-plugin/commit/d313460afbc15d15b2dc63c1f48e29c35e2c16dd))
* support non csidriver plugin ([d209f05](https://github.com/sergelogvinov/hybrid-csi-plugin/commit/d209f05c5bf64f1fb210fac92a71eb4d24cff885))


### Bug Fixes

* helm chart deploy ([89f40f6](https://github.com/sergelogvinov/hybrid-csi-plugin/commit/89f40f6d2c7cedd0929b04c9ef945a1817f3881e))
* linter checks ([006d804](https://github.com/sergelogvinov/hybrid-csi-plugin/commit/006d804849981a87ac2bbd0b50e031a9fba9b3f3))
