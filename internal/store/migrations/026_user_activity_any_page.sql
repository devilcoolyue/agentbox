-- 最后活跃从「只算终端页面」改为全站页面的点击和输入都算，列名跟着改。
ALTER TABLE user_activity RENAME COLUMN last_terminal_at TO last_active_at;
