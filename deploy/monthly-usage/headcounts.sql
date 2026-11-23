-- 注册时间以北京时间下月初为开区间上界。状态、处罚和部门均取导出事务的快照。
-- SecurityPenaltyService.Admission 按当前配置和用户豁免决定是否执行 user_block。
-- 分组只影响违规记次；会话、IP、Key、平台或模型限制均不封禁整个人。
WITH penalty_config AS (
    SELECT NULLIF(value, '')::jsonb AS cfg
    FROM settings
    WHERE key = 'security_penalty_config'
), trusted_users AS (
    -- ParseCyberPolicyUserAllowlist 接受逗号或 Unicode 空白分隔的十进制 ID。
    SELECT token::bigint AS user_id
    FROM settings
    CROSS JOIN LATERAL regexp_split_to_table(value COLLATE "C",
        U&'[,\0009-\000D\0020\0085\00A0\1680\2000-\200A\2028\2029\202F\205F\3000]+') AS token
    WHERE key = 'cyber_policy_user_allowlist' AND token <> ''
), blocked_users AS (
    SELECT s.user_id
    FROM security_penalty_subjects s
    CROSS JOIN penalty_config
    WHERE s.user_id > 0 AND s.scope = 'user' AND s.subject_key = 'user' AND s.policy = 'user_block'
      AND s.strike_count > 0 AND s.blocked_until > CURRENT_TIMESTAMP
      AND cfg->>'policy' = 'user_block'
      AND cfg->>'enabled' = 'true' AND cfg->>'observe_only' = 'false'
      AND EXISTS (SELECT 1 FROM settings WHERE key = 'risk_control_enabled' AND value = 'true')
      AND (cfg->'exempt_user_ids' @> jsonb_build_array(s.user_id)) IS NOT TRUE
      AND NOT EXISTS (SELECT 1 FROM trusted_users t WHERE t.user_id = s.user_id)
), departments AS (
    SELECT DISTINCT ON (v.user_id) v.user_id,
           COALESCE(CASE WHEN k.type = 'select' THEN (
               SELECT btrim(opt->>'label')
               FROM jsonb_array_elements(CASE WHEN jsonb_typeof(k.options) = 'array' THEN k.options END) AS opt
               WHERE opt->>'value' = v.value AND opt->>'label' ~ '[^[:space:]]'
               LIMIT 1
           ) END, btrim(v.value)) AS department
    FROM user_attribute_definitions k
    JOIN user_attribute_values v ON v.attribute_id = k.id
    CROSS JOIN (SELECT 0::bigint AS department_attribute_id) cfg
    WHERE k.deleted_at IS NULL AND v.value ~ '[^[:space:]]'
      AND CASE WHEN cfg.department_attribute_id = 0 THEN k.key IN ('dingtalk_department', 'department')
               ELSE k.id = cfg.department_attribute_id END
    ORDER BY v.user_id, (k.key = 'dingtalk_department') DESC
), counts AS (
    SELECT COALESCE(d.department, '') AS department, count(*) AS headcount
    FROM users u
    LEFT JOIN departments d ON d.user_id = u.id
    WHERE u.created_at < ((:'month'::date + interval '1 month') AT TIME ZONE 'Asia/Shanghai')
      AND u.deleted_at IS NULL AND u.status = 'active'
      AND u.role IS DISTINCT FROM 'admin'
      AND NOT EXISTS (SELECT 1 FROM blocked_users b WHERE b.user_id = u.id)
    GROUP BY COALESCE(d.department, '')
)
SELECT COALESCE(jsonb_object_agg(department, headcount), '{}'::jsonb) FROM counts;
