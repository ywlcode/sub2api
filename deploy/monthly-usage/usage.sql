-- psql -X -q -v ON_ERROR_STOP=1 -v month=YYYY-MM-01 -f - < usage.sql
-- usage_day 为 usage_logs.created_at 的北京时间日期，范围从指定月初到下月初（右端不含）。
-- user_id 来自日志，按本站导出时角色排除管理员（含软删除）；username 来自导出时的 users。名称或分组缺失时保留用量行。
-- model 为客户端请求模型，按后台模型统计口径，去除 requested_model 两端空格后若为空则用 model。
-- used_groups 为本行日志 group_id 对应的组名，包含仍可查到的已软删除组。组名取导出时的值。
-- 组名去重、排序，用逗号和空格连接。
-- requests 按日志行计数。四类 token 分别为日志中的输入、输出、缓存写入、缓存读取；
-- total_tokens 与 UsageLog.TotalTokens 一致，四项相加，不再叠加缓存时长细分或图像 token。
-- actual_cost 累计请求时的用户计费金额（USD，保留十进制精度），包含订阅用量。
-- profile_name 取 dingtalk_name；department 优先取 dingtalk_department，再取 department。
-- 仅从未删除的属性定义读取这三个键。用户名或姓名本身为邮箱时，该列留空。
\set email_pattern '^[[:space:]]*(".*"|[^@[:space:]]+)@[^@[:space:]]+[[:space:]]*$'
COPY (
    WITH daily_usage AS (
        SELECT (l.created_at AT TIME ZONE 'Asia/Shanghai')::date AS usage_day,
               l.user_id,
               COALESCE(NULLIF(btrim(l.requested_model), ''), l.model) AS model,
               array_agg(DISTINCT l.group_id) FILTER (WHERE l.group_id IS NOT NULL) AS group_ids,
               count(*) AS requests,
               sum(l.input_tokens) AS input_tokens,
               sum(l.output_tokens) AS output_tokens,
               sum(l.cache_creation_tokens) AS cache_creation_tokens,
               sum(l.cache_read_tokens) AS cache_read_tokens,
               sum(l.actual_cost) AS actual_cost
        FROM usage_logs l
        WHERE l.created_at >= (:'month'::date::timestamp AT TIME ZONE 'Asia/Shanghai')
          AND l.created_at < ((:'month'::date + interval '1 month') AT TIME ZONE 'Asia/Shanghai')
          AND NOT EXISTS (SELECT 1 FROM users a WHERE a.id = l.user_id AND a.role = 'admin')
        GROUP BY 1, 2, 3
    ), group_names AS (
        SELECT id, NULLIF(name, '')::text COLLATE "C" AS name FROM groups
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
    ), profile_names AS (
        SELECT v.user_id, NULLIF(btrim(v.value), '') AS profile_name
        FROM user_attribute_definitions k
        JOIN user_attribute_values v ON v.attribute_id = k.id
        WHERE k.deleted_at IS NULL AND k.key = 'dingtalk_name'
    )
    SELECT to_char(d.usage_day, 'YYYY-MM-DD') AS usage_day, d.user_id,
           CASE WHEN u.username !~ :'email_pattern' THEN u.username END AS username,
           CASE WHEN p.profile_name !~ :'email_pattern' THEN p.profile_name END AS profile_name,
           dept.department,
           (SELECT string_agg(DISTINCT g.name, ', ' ORDER BY g.name)
            FROM group_names g WHERE g.id = ANY(d.group_ids)) AS used_groups,
           d.model, d.requests, d.input_tokens, d.output_tokens, d.cache_read_tokens,
           d.input_tokens + d.output_tokens + d.cache_creation_tokens + d.cache_read_tokens AS total_tokens,
           d.actual_cost
    FROM daily_usage d
    LEFT JOIN users u ON u.id = d.user_id
    LEFT JOIN profile_names p ON p.user_id = d.user_id
    LEFT JOIN departments dept ON dept.user_id = d.user_id
    ORDER BY d.usage_day, d.user_id, d.model COLLATE "C"
) TO STDOUT WITH CSV HEADER;
