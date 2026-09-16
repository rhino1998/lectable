package com.lectable.app.data.remote.dto

import kotlinx.serialization.Serializable

/** Mirrors backend/internal/httpapi/instance.go's handleGetInstance. [id] is a stable per-backend
 *  identity (survives restarts/network-address changes) used to scope locally-cached data, since
 *  the app can be pointed at different backends over its lifetime (see android/CLAUDE.md's
 *  "Server address") and a book id alone is only unique within one backend's own database.
 *  [name] is a purely cosmetic, optional operator-set label (LIBRARY_NAME) for telling backends
 *  apart in the UI - "" if the deployer never set one, never used for cache scoping. */
@Serializable
data class InstanceDto(val id: String, val name: String = "")
