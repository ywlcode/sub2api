INSERT INTO users (id, username, email, password_hash) VALUES
    (1, E'张,"三"\n甲', 'test@example.invalid', 'unused'),
    (2, '', '', 'unused'),
    (3, '无用量', 'idle@example.invalid', 'unused'),
    (4, '无属性', 'plain@example.invalid', 'unused');
INSERT INTO user_attribute_definitions (id, key, name, type, deleted_at) VALUES
    (1, 'dingtalk_name', '姓名', 'text', NULL),
    (2, 'dingtalk_department', '组织路径', 'text', NULL),
    (3, 'department', '部门', 'text', NULL),
    (4, 'dingtalk_name', '旧姓名', 'text', now()),
    (5, 'dingtalk_email', '其他邮箱', 'email', NULL);
INSERT INTO user_attribute_values (user_id, attribute_id, value) VALUES
    (1, 1, '钉钉姓名'), (1, 2, '公司/研发'), (1, 3, '备用部门'),
    (1, 4, '过期姓名'), (1, 5, 'excluded@example.invalid'),
    (2, 2, '   '), (2, 3, '运维');
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
    (4, 4, 'fixture-four', 'four', NULL);
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
    (1,1,1,'2024-02-01 16:00Z','client',NULL,11,       6,7,8,9,0,0),
    (1,1,1,'2024-02-29 15:59:59.999999Z','client',NULL,NULL, 1,1,1,1,0,0),
    (1,1,1,'2024-02-29 16:00Z','outside',NULL,11,       8,8,8,8,8,8);
UPDATE usage_logs SET upstream_model = 'upstream', cache_creation_5m_tokens = 3,
                      image_input_tokens = 100, image_output_tokens = 200
WHERE model = 'mapped' AND group_id = 12;
