use std::path::{Path, PathBuf};

const MEDIA_TYPE_PHOTO: &str = "photo";
const MEDIA_TYPE_DOCUMENT: &str = "document";
const MEDIA_TYPE_VIDEO: &str = "video";
const MEDIA_TYPE_ANIMATION: &str = "animation";
const MEDIA_TYPE_AUDIO: &str = "audio";
const MEDIA_TYPE_VOICE: &str = "voice";
const MEDIA_TYPE_OTHER: &str = "other";

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct MediaInfo {
    pub message_id: i64,
    pub td_file_id: i32,
    pub media_type: String,
    pub file_name: Option<String>,
    pub file_size: i64,
    pub mime_type: String,
    pub chat_id: i64,
    pub task_id: Option<String>,
}

impl MediaInfo {
    pub fn default_file_name(&self) -> String {
        let ext = extension_for_mime(&self.mime_type);
        format!("file_{}_{}{}", self.message_id, self.td_file_id, ext)
    }
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PathPlan {
    pub directory: PathBuf,
    pub file_name: String,
    pub file_path: PathBuf,
}

pub fn classify_dir(media_type: &str) -> &'static str {
    match media_type {
        MEDIA_TYPE_PHOTO | MEDIA_TYPE_DOCUMENT | MEDIA_TYPE_VIDEO | MEDIA_TYPE_ANIMATION
        | MEDIA_TYPE_AUDIO | MEDIA_TYPE_VOICE => media_type_name(media_type),
        _ => MEDIA_TYPE_OTHER,
    }
}

const fn media_type_name(media_type: &str) -> &'static str {
    match media_type.as_bytes() {
        b"photo" => MEDIA_TYPE_PHOTO,
        b"document" => MEDIA_TYPE_DOCUMENT,
        b"video" => MEDIA_TYPE_VIDEO,
        b"animation" => MEDIA_TYPE_ANIMATION,
        b"audio" => MEDIA_TYPE_AUDIO,
        b"voice" => MEDIA_TYPE_VOICE,
        _ => MEDIA_TYPE_OTHER,
    }
}

pub fn extension_for_mime(mime_type: &str) -> &'static str {
    match mime_type {
        "image/jpeg" => ".jpg",
        "image/png" => ".png",
        "image/gif" => ".gif",
        "image/webp" => ".webp",
        "video/mp4" => ".mp4",
        "video/avi" => ".avi",
        "video/mov" => ".mov",
        "video/webm" => ".webm",
        "audio/mp3" => ".mp3",
        "audio/ogg" => ".ogg",
        "application/pdf" => ".pdf",
        _ => "",
    }
}

pub fn sanitize_file_name(file_name: &str) -> String {
    let mut clean = file_name.replace('/', "_");
    clean = clean.replace('\\', "_");
    clean = clean.replace("..", "_");
    clean = clean.replace(':', "_");
    clean = clean.replace('*', "_");
    clean = clean.replace('?', "_");
    clean = clean.replace('"', "_");
    clean = clean.replace('<', "_");
    clean = clean.replace('>', "_");
    clean = clean.replace('|', "_");

    if clean.is_empty() || clean == "." || clean == ".." {
        "unnamed_file".to_owned()
    } else {
        clean
    }
}

pub fn plan_media_path(
    download_path: impl AsRef<Path>,
    media: &MediaInfo,
    classify_by_type: bool,
) -> PathPlan {
    let mut directory = download_path
        .as_ref()
        .join(format!("chat_{}", media.chat_id));
    if classify_by_type {
        directory = directory.join(classify_dir(&media.media_type));
    }

    let raw_name = media
        .file_name
        .as_deref()
        .filter(|name| !name.is_empty())
        .map(str::to_owned)
        .unwrap_or_else(|| media.default_file_name());
    let file_name = sanitize_file_name(&raw_name);
    let file_path = directory.join(&file_name);

    PathPlan {
        directory,
        file_name,
        file_path,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn media(file_name: Option<&str>, media_type: &str, mime_type: &str) -> MediaInfo {
        MediaInfo {
            message_id: 42,
            td_file_id: 7,
            media_type: media_type.to_owned(),
            file_name: file_name.map(str::to_owned),
            file_size: 1024,
            mime_type: mime_type.to_owned(),
            chat_id: 100,
            task_id: None,
        }
    }

    #[test]
    fn classify_dir_matches_go_downloader() {
        assert_eq!(classify_dir("photo"), "photo");
        assert_eq!(classify_dir("document"), "document");
        assert_eq!(classify_dir("video"), "video");
        assert_eq!(classify_dir("animation"), "animation");
        assert_eq!(classify_dir("audio"), "audio");
        assert_eq!(classify_dir("voice"), "voice");
        assert_eq!(classify_dir("sticker"), "other");
        assert_eq!(classify_dir(""), "other");
    }

    #[test]
    fn extension_for_mime_matches_go_downloader() {
        assert_eq!(extension_for_mime("image/jpeg"), ".jpg");
        assert_eq!(extension_for_mime("image/png"), ".png");
        assert_eq!(extension_for_mime("video/mp4"), ".mp4");
        assert_eq!(extension_for_mime("application/pdf"), ".pdf");
        assert_eq!(extension_for_mime("application/octet-stream"), "");
    }

    #[test]
    fn sanitize_file_name_matches_go_downloader() {
        assert_eq!(
            sanitize_file_name("../a:b*c?d\"e<f>g|h\\i/j"),
            "__a_b_c_d_e_f_g_h_i_j"
        );
        assert_eq!(sanitize_file_name(""), "unnamed_file");
        assert_eq!(sanitize_file_name("."), "unnamed_file");
        assert_eq!(sanitize_file_name(".."), "_");
    }

    #[test]
    fn plan_media_path_uses_classification_when_enabled() {
        let media = media(Some("a.jpg"), "photo", "image/jpeg");
        let plan = plan_media_path("downloads", &media, true);

        assert_eq!(
            plan.directory,
            PathBuf::from("downloads").join("chat_100").join("photo")
        );
        assert_eq!(plan.file_name, "a.jpg");
        assert_eq!(
            plan.file_path,
            PathBuf::from("downloads")
                .join("chat_100")
                .join("photo")
                .join("a.jpg")
        );
    }

    #[test]
    fn plan_media_path_can_use_flat_chat_directory() {
        let media = media(Some("a.jpg"), "photo", "image/jpeg");
        let plan = plan_media_path("downloads", &media, false);

        assert_eq!(plan.directory, PathBuf::from("downloads").join("chat_100"));
        assert_eq!(
            plan.file_path,
            PathBuf::from("downloads").join("chat_100").join("a.jpg")
        );
    }

    #[test]
    fn plan_media_path_generates_default_file_name() {
        let media = media(None, "document", "application/pdf");
        let plan = plan_media_path("downloads", &media, true);

        assert_eq!(plan.file_name, "file_42_7.pdf");
        assert_eq!(
            plan.file_path,
            PathBuf::from("downloads")
                .join("chat_100")
                .join("document")
                .join("file_42_7.pdf")
        );
    }
}
