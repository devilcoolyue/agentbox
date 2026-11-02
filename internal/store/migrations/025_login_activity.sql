-- 管理员「登录与活跃」面板的数据（store/login_activity.go）。
-- login_events 每次发放登录令牌记一行：密码登录、桌面端配对、abox-link 配对。
-- user_activity 是用户最后一次在终端页面人为操作（点击、输入）的时间，Agent
-- 自己运行、终端自动应答都不会更新它。时间都是 Unix 毫秒，排序和清理不依赖文本格式。
CREATE TABLE login_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user TEXT NOT NULL,
    at INTEGER NOT NULL,
    method TEXT NOT NULL,
    ip TEXT NOT NULL DEFAULT '',
    user_agent TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_login_events_user ON login_events(user, id);
CREATE INDEX idx_login_events_at ON login_events(at);

CREATE TABLE user_activity (
    user TEXT PRIMARY KEY,
    last_terminal_at INTEGER NOT NULL
);
