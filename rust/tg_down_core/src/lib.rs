//! Pure business rules for the Tg-Down Rust helper.
//!
//! Scope is deliberately limited to media path planning — the one rule the Go
//! application actually invokes through the `tg-down-core` subprocess bridge.
//! It mirrors Go's downloader path logic so the two stay byte-for-byte identical.

pub mod media;

pub use media::{
    classify_dir, extension_for_mime, plan_media_path, sanitize_file_name, MediaInfo, PathPlan,
};
