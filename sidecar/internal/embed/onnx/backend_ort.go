// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build ORT

package onnx // import "miniflux.app/v2/sidecar/internal/embed/onnx"

// BackendName reports which inference backend this binary was compiled with.
func BackendName() string { return "ORT" }
