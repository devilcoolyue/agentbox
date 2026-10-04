use crate::{
    remote::{valid_session_id, Error, Result},
    Desktop,
};
use reqwest::Method;
use serde::{Deserialize, Serialize};
use tauri::State;

#[derive(Serialize, Deserialize)]
pub struct Project {
    pub id: String,
    pub session_id: String,
    pub name: String,
    pub path: String,
    pub arguments: Vec<String>,
    pub revision: i64,
    pub created_at: String,
}

#[derive(Serialize, Deserialize)]
pub struct ProjectTerminal {
    pub id: String,
    pub session_id: String,
    pub project_id: String,
    pub kind: String,
    pub state: String,
    pub arguments: Vec<String>,
    pub created_at: String,
}

async fn remote(state: &Desktop) -> Result<std::sync::Arc<crate::Remote>> {
    let data = state.inner.lock().await;
    if data
        .connection
        .as_ref()
        .and_then(|c| c.capabilities.as_ref())
        .is_none_or(|c| c.features.project_terminals != 1)
    {
        return Err(Error::new(
            "unsupported",
            "此服务端不支持项目独立终端协议 v1",
        ));
    }
    data.remote
        .clone()
        .ok_or_else(|| Error::new("unauthorized", "请先登录"))
}

#[tauri::command]
pub async fn list_projects(state: State<'_, Desktop>, session: String) -> Result<Vec<Project>> {
    valid_session_id(&session)?;
    remote(&state)
        .await?
        .api(
            Method::GET,
            &format!("api/sessions/{session}/client-projects"),
            None,
        )
        .await
}

#[derive(Deserialize)]
pub struct ProjectEdit {
    pub name: String,
    pub path: String,
    pub arguments: Vec<String>,
    pub revision: Option<i64>,
}

#[tauri::command]
pub async fn save_project(
    state: State<'_, Desktop>,
    session: String,
    id: Option<String>,
    edit: ProjectEdit,
) -> Result<Project> {
    valid_session_id(&session)?;
    let (method, path) = if let Some(id) = id {
        valid_session_id(&id)?;
        (
            Method::PUT,
            format!("api/sessions/{session}/client-projects/{id}"),
        )
    } else {
        (
            Method::POST,
            format!("api/sessions/{session}/client-projects"),
        )
    };
    remote(&state).await?.api(method,&path,Some(serde_json::json!({"name":edit.name,"path":edit.path,"arguments":edit.arguments,"revision":edit.revision.unwrap_or(0)}))).await
}

#[tauri::command]
pub async fn delete_project(
    state: State<'_, Desktop>,
    session: String,
    id: String,
    revision: i64,
) -> Result<()> {
    valid_session_id(&session)?;
    valid_session_id(&id)?;
    let _: serde_json::Value = remote(&state)
        .await?
        .api(
            Method::DELETE,
            &format!("api/sessions/{session}/client-projects/{id}?revision={revision}"),
            None,
        )
        .await?;
    Ok(())
}

#[tauri::command]
pub async fn list_terminals(
    state: State<'_, Desktop>,
    session: String,
) -> Result<Vec<ProjectTerminal>> {
    valid_session_id(&session)?;
    remote(&state)
        .await?
        .api(
            Method::GET,
            &format!("api/sessions/{session}/client-terminals"),
            None,
        )
        .await
}

#[tauri::command]
pub async fn create_terminal(
    state: State<'_, Desktop>,
    session: String,
    project: String,
    kind: String,
) -> Result<ProjectTerminal> {
    valid_session_id(&session)?;
    valid_session_id(&project)?;
    if !matches!(kind.as_str(), "agent" | "shell") {
        return Err(Error::new("request", "请选择 AI 或 Shell 终端"));
    }
    remote(&state)
        .await?
        .api(
            Method::POST,
            &format!("api/sessions/{session}/client-terminals"),
            Some(serde_json::json!({"project_id":project,"kind":kind})),
        )
        .await
}

#[tauri::command]
pub async fn end_terminal(state: State<'_, Desktop>, session: String, id: String) -> Result<()> {
    valid_session_id(&session)?;
    valid_session_id(&id)?;
    let _: serde_json::Value = remote(&state)
        .await?
        .api(
            Method::DELETE,
            &format!("api/sessions/{session}/client-terminals/{id}"),
            None,
        )
        .await?;
    Ok(())
}
