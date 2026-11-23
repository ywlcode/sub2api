-- psql -X -q -v ON_ERROR_STOP=1 -v month=YYYY-MM-01 -f - < usage.sql
-- usage_day 为 usage_logs.created_at 的北京时间日期，范围从指定月初到下月初（右端不含）。
-- user_id 来自日志；username、email 为导出时 users 中的值。名称或分组缺失时保留用量行。
-- model 为客户端请求模型，按后台模型统计口径，去除 requested_model 两端空格后若为空则用 model。
-- used_groups 为本行日志 group_id 对应的组名，包含仍可查到的已软删除组。组名取导出时的值。
-- allowed_groups 为导出时 user_allowed_groups 显式关联的未删除组，不展开公共组。
-- subscription_groups 取导出时的有效订阅，沿用 ListActiveByUserID 的 active、未过期条件，
-- 排除已软删除的订阅和组。三列组名均去重、排序，用逗号和空格连接。
-- requests 按日志行计数。四类 token 分别为日志中的输入、输出、缓存写入、缓存读取；
-- total_tokens 与 UsageLog.TotalTokens 一致，四项相加，不再叠加缓存时长细分或图像 token。
-- total_cost、actual_cost 分别累计日志中的原始费用和用户计费金额（USD，保留十进制精度）。
-- 两者均为请求时的记录，actual_cost 包含订阅用量，不等同于钱包流水。
-- profile_name 取 dingtalk_name；department 优先取 dingtalk_department，再取 department。
-- 仅从未删除的属性定义读取这三个键。空值留空，账户用户名和邮箱始终取自 users。
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
               sum(l.total_cost) AS total_cost,
               sum(l.actual_cost) AS actual_cost
        FROM usage_logs l
        WHERE l.created_at >= (:'month'::date::timestamp AT TIME ZONE 'Asia/Shanghai')
          AND l.created_at < ((:'month'::date + interval '1 month') AT TIME ZONE 'Asia/Shanghai')
        GROUP BY 1, 2, 3
    ), group_names AS NOT MATERIALIZED (
        SELECT id, NULLIF(name, '')::text COLLATE "C" AS name, deleted_at FROM groups
    ), allowed AS (
        SELECT a.user_id, string_agg(DISTINCT g.name, ', ' ORDER BY g.name) AS names
        FROM user_allowed_groups a
        JOIN group_names g ON g.id = a.group_id AND g.deleted_at IS NULL
        GROUP BY a.user_id
    ), subscriptions AS (
        SELECT s.user_id, string_agg(DISTINCT g.name, ', ' ORDER BY g.name) AS names
        FROM user_subscriptions s
        JOIN group_names g ON g.id = s.group_id AND g.deleted_at IS NULL
        WHERE s.deleted_at IS NULL AND s.status = 'active' AND s.expires_at > CURRENT_TIMESTAMP
        GROUP BY s.user_id
    ), profiles AS (
        SELECT v.user_id,
               max(NULLIF(btrim(v.value), '')) FILTER (WHERE k.key = 'dingtalk_name') AS profile_name,
               COALESCE(
                   max(NULLIF(btrim(v.value), '')) FILTER (WHERE k.key = 'dingtalk_department'),
                   max(NULLIF(btrim(v.value), '')) FILTER (WHERE k.key = 'department')
               ) AS department
        FROM user_attribute_definitions k
        JOIN user_attribute_values v ON v.attribute_id = k.id
        WHERE k.deleted_at IS NULL AND k.key IN ('dingtalk_name', 'dingtalk_department', 'department')
        GROUP BY v.user_id
    )
    SELECT to_char(d.usage_day, 'YYYY-MM-DD') AS usage_day, d.user_id, u.username, u.email,
           (SELECT string_agg(DISTINCT g.name, ', ' ORDER BY g.name)
            FROM group_names g WHERE g.id = ANY(d.group_ids)) AS used_groups,
           a.names AS allowed_groups, s.names AS subscription_groups,
           d.model, d.requests, d.input_tokens, d.output_tokens, d.cache_creation_tokens, d.cache_read_tokens,
           d.input_tokens + d.output_tokens + d.cache_creation_tokens + d.cache_read_tokens AS total_tokens,
           d.total_cost, d.actual_cost, p.profile_name, p.department
    FROM daily_usage d
    LEFT JOIN users u ON u.id = d.user_id
    LEFT JOIN allowed a ON a.user_id = d.user_id
    LEFT JOIN subscriptions s ON s.user_id = d.user_id
    LEFT JOIN profiles p ON p.user_id = d.user_id
    ORDER BY d.usage_day, d.user_id, d.model COLLATE "C"
) TO STDOUT WITH CSV HEADER;
