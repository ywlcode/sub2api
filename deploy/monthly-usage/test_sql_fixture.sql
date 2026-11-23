INSERT INTO users (id, username, email, password_hash) VALUES
    (1, E'张,"三"\n甲', 'test@example.invalid', 'unused'),
    (2, '', '', 'unused'),
    (3, '无用量', 'idle@example.invalid', 'unused'),
    (4, '无属性', 'plain@example.invalid', 'unused'),
    (5, E'\tAlice+dev@Example.invalid ', 'account5@example.invalid', 'unused'),
    (6, '@小周', 'account6@example.invalid', 'unused');
UPDATE users SET created_at = CASE
    WHEN id = 3 THEN '2024-02-29 15:59:59.999999Z'::timestamptz
    WHEN id IN (5, 6) THEN '2024-05-01 00:00Z'::timestamptz
    ELSE '2023-01-01 00:00Z'::timestamptz END;
INSERT INTO users (id, email, password_hash, created_at, deleted_at, status, role) VALUES
    (7, 'removed@example.invalid', 'unused', '2024-01-01Z', '2024-03-15Z', 'active', 'user'),
    (8, 'disabled@example.invalid', 'unused', '2024-01-01Z', NULL, 'disabled', 'user'),
    (9, 'march@example.invalid', 'unused', '2024-02-29 16:00Z', NULL, 'active', 'user'),
    (10, 'blocked@example.invalid', 'unused', '2024-01-01Z', NULL, 'active', 'user'),
    (11, 'expired@example.invalid', 'unused', '2024-01-01Z', NULL, 'active', 'user'),
    (12, 'session@example.invalid', 'unused', '2024-01-01Z', NULL, 'active', 'user'),
    (13, 'exempt@example.invalid', 'unused', '2024-01-01Z', NULL, 'active', 'user'),
    (14, 'trusted@example.invalid', 'unused', '2024-01-01Z', NULL, 'active', 'user'),
    (15, 'admin@example.invalid', 'unused', '2024-01-01Z', NULL, 'active', 'admin'),
    (16, 'only-admin@example.invalid', 'unused', '2024-01-01Z', NULL, 'active', 'admin'),
    (17, 'deleted-admin@example.invalid', 'unused', '2024-01-01Z', '2024-03-15Z', 'active', 'admin');
INSERT INTO user_attribute_definitions (id, key, name, type, deleted_at) VALUES
    (1, 'dingtalk_name', '姓名', 'text', NULL),
    (2, 'dingtalk_department', '组织路径', 'select', NULL),
    (3, 'department', '部门', 'text', NULL),
    (4, 'dingtalk_name', '旧姓名', 'text', now()),
    (5, 'dingtalk_email', '其他邮箱', 'email', NULL),
    (6, 'dingtalk_department', '旧组织', 'text', now());
UPDATE user_attribute_definitions
SET options = '[{"value":" rd ","label":"  公司/研发  "}]' WHERE id = 2;
INSERT INTO user_attribute_values (user_id, attribute_id, value) VALUES
    (1, 1, '钉钉姓名'), (1, 2, ' rd '), (1, 3, '备用部门'),
    (1, 4, '过期姓名'), (1, 5, 'excluded@example.invalid'),
    (2, 2, '   '), (2, 3, '运维'),
    (5, 1, '小林'), (5, 3, ' 公司/研发 '),
    (6, 2, ' old-option '), (6, 3, '不应回退'), (6, 1, E' "Zhou Li"@example.invalid\n');
INSERT INTO user_attribute_values (user_id, attribute_id, value) VALUES
    (2, 6, '已删组织'), (3, 3, E'  仅注册"部门"\n二组  '),
    (7, 3, '已删'), (8, 3, '停用'), (9, 3, '三月新增'), (10, 3, '网关封禁'),
    (11, 3, '处罚到期'), (12, 3, '会话限制'), (13, 3, '运维'), (14, 3, '运维'), (15, 3, '运维'),
    (16, 3, '纯管理员部门'), (17, 3, '纯管理员部门');
UPDATE settings SET value =
    '{"enabled":true,"observe_only":false,"policy":"user_block","exempt_user_ids":[13]}'
    WHERE key = 'security_penalty_config';
INSERT INTO settings (key, value) VALUES
    ('risk_control_enabled', 'true'), ('cyber_policy_user_allowlist', U&'99,\3000+014');
INSERT INTO security_penalty_subjects (user_id, scope, subject_key, policy, strike_count, blocked_until)
VALUES (10, 'user', 'user', 'user_block', 3, now() + interval '1 day'),
       (11, 'user', 'user', 'user_block', 3, '2024-03-02Z'),
       (12, 'session', 'fixture-session', 'session_block', 3, now() + interval '1 day'),
       (13, 'user', 'user', 'user_block', 3, now() + interval '1 day'),
       (14, 'user', 'user', 'user_block', 3, now() + interval '1 day'),
       (15, 'user', 'user', 'user_block', 3, now() + interval '1 day');
INSERT INTO groups (id, name) VALUES
    (10, 'B'), (11, 'A'), (12, 'old'), (13, 'plan-A'), (14, 'plan-B'),
    (15, 'expired'), (16, 'suspended'), (17, 'removed');
UPDATE groups SET deleted_at = now() WHERE id = 12;
INSERT INTO user_allowed_groups (user_id, group_id) VALUES (1, 11), (1, 10), (1, 12);
INSERT INTO user_subscriptions (user_id, group_id, starts_at, expires_at, status, deleted_at)
VALUES (1, 14, now() - interval '7 days', 'infinity', 'active', NULL),
       (1, 13, now() - interval '7 days', 'infinity', 'active', NULL),
       (1, 12, now() - interval '7 days', 'infinity', 'active', NULL),
       (1, 15, now() - interval '7 days', now() - interval '1 day', 'active', NULL),
       (1, 16, now() - interval '7 days', 'infinity', 'suspended', NULL),
       (1, 17, now() - interval '7 days', 'infinity', 'active', now());

INSERT INTO accounts (id, name, platform, type) VALUES (1, 'fixture', 'openai', 'apikey');
INSERT INTO api_keys (id, user_id, key, name, group_id) VALUES
    (1, 1, 'fixture-one', 'one', 11), (2, 2, 'fixture-two', 'two', NULL),
    (4, 4, 'fixture-four', 'four', NULL), (5, 5, 'fixture-five', 'five', NULL),
    (6, 6, 'fixture-six', 'six', NULL), (7, 7, 'fixture-seven', 'seven', NULL),
    (15, 15, 'fixture-admin', 'admin', NULL), (16, 16, 'fixture-only-admin', 'admin', NULL),
    (17, 17, 'fixture-deleted-admin', 'admin', NULL);
INSERT INTO usage_logs (
    user_id, api_key_id, account_id, created_at, model, requested_model, group_id,
    input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, total_cost, actual_cost
) VALUES
    (1,1,1,'2024-01-31 15:59:59.999999Z','outside',NULL,11, 7,7,7,7,7,7),
    (1,1,1,'2024-01-31 16:00Z','mapped',' client ',12, 2147483647,1,3,4,1.0000000001,0.0000000001),
    (1,1,1,'2024-02-01 03:00Z','client',NULL,10,       2,3,4,5,0.2000000002,0.1000000002),
    (1,1,1,'2024-02-01 15:59:59.999999Z','client','   ',10, 1,2,0,4,0.3,0.15),
    (1,1,1,'2024-02-01 03:01Z','mapped','other',NULL,  0,0,0,0,1.5,0),
    (2,2,1,'2024-02-01 01:00Z','client','',NULL,       0,0,0,0,0,0),
    (4,4,1,'2024-02-01 01:00Z','client',NULL,NULL,     0,0,0,0,0,0),
    (7,7,1,'2024-02-01 01:00Z','client',NULL,NULL,     1,2,3,4,5,6),
    (15,15,1,'2024-02-01 01:00Z','client',NULL,NULL,   101,102,103,104,105,106),
    (16,16,1,'2024-02-01 01:00Z','client',NULL,NULL,   101,102,103,104,105,106),
    (17,17,1,'2024-02-01 01:00Z','client',NULL,NULL,   101,102,103,104,105,106),
    (1,1,1,'2024-02-01 16:00Z','client',NULL,11,       6,7,8,9,0,0),
    (1,1,1,'2024-02-29 15:59:59.999999Z','client',NULL,NULL, 1,1,1,1,0,0),
    (1,1,1,'2024-02-29 16:00Z','outside',NULL,11,       8,8,8,8,8,8),
    (5,5,1,'2024-05-01 01:00Z','client',NULL,NULL,      0,0,0,0,0,0),
    (6,6,1,'2024-05-01 01:00Z','client',NULL,NULL,      0,0,0,0,0,0);
UPDATE usage_logs SET upstream_model = 'upstream', cache_creation_5m_tokens = 3,
                      image_input_tokens = 100, image_output_tokens = 200
WHERE model = 'mapped' AND group_id = 12;

-- 使用列级授权执行导出，使读取邮箱、原始费用或权限/订阅关系的查询在测试时失败。
CREATE ROLE report_reader LOGIN;
GRANT SELECT (created_at, user_id, requested_model, model, group_id, input_tokens,
              output_tokens, cache_creation_tokens, cache_read_tokens, actual_cost)
    ON usage_logs TO report_reader;
GRANT SELECT (id, username, created_at, deleted_at, status, role) ON users TO report_reader;
GRANT SELECT (user_id, scope, subject_key, policy, strike_count, blocked_until)
    ON security_penalty_subjects TO report_reader;
GRANT SELECT (key, value) ON settings TO report_reader;
GRANT SELECT (id, name) ON groups TO report_reader;
GRANT SELECT (id, key, type, options, deleted_at) ON user_attribute_definitions TO report_reader;
GRANT SELECT (user_id, attribute_id, value) ON user_attribute_values TO report_reader;
