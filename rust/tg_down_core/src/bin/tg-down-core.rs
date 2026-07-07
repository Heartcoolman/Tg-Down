use serde::{Deserialize, Serialize};
use std::io::{self, Read};
use tg_down_core::{plan_media_path, MediaInfo};

/// 协议版本，必须与 Go 侧 rustcore.protocolVersion 保持一致。
const PROTOCOL_VERSION: i32 = 1;

#[derive(Debug, Deserialize)]
struct Request {
    op: String,
    #[serde(default)]
    protocol_version: i32,
    download_path: String,
    classify_by_type: bool,
    media: MediaRequest,
}

#[derive(Debug, Deserialize)]
struct MediaRequest {
    message_id: i64,
    td_file_id: i32,
    media_type: String,
    file_name: Option<String>,
    file_size: i64,
    mime_type: String,
    chat_id: i64,
    task_id: Option<String>,
}

#[derive(Debug, Serialize)]
struct PathPlanResponse {
    protocol_version: i32,
    directory: String,
    file_name: String,
    file_path: String,
}

fn main() {
    if let Err(err) = run() {
        eprintln!("{err}");
        std::process::exit(1);
    }
}

fn run() -> Result<(), Box<dyn std::error::Error>> {
    let mut input = String::new();
    io::stdin().read_to_string(&mut input)?;
    let req: Request = serde_json::from_str(&input)?;

    if req.protocol_version != PROTOCOL_VERSION {
        return Err(format!(
            "protocol version mismatch: helper {PROTOCOL_VERSION}, request {}",
            req.protocol_version
        )
        .into());
    }

    match req.op.as_str() {
        "plan_media_path" => {
            let media = MediaInfo {
                message_id: req.media.message_id,
                td_file_id: req.media.td_file_id,
                media_type: req.media.media_type,
                file_name: req.media.file_name,
                file_size: req.media.file_size,
                mime_type: req.media.mime_type,
                chat_id: req.media.chat_id,
                task_id: req.media.task_id,
            };
            let plan = plan_media_path(req.download_path, &media, req.classify_by_type);
            let resp = PathPlanResponse {
                protocol_version: PROTOCOL_VERSION,
                directory: plan.directory.to_string_lossy().into_owned(),
                file_name: plan.file_name,
                file_path: plan.file_path.to_string_lossy().into_owned(),
            };
            println!("{}", serde_json::to_string(&resp)?);
            Ok(())
        }
        other => Err(format!("unknown op: {other}").into()),
    }
}
