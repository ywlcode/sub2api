export default {
  avgUserUpload: '用户上传平均耗时',
  avgProcessing: '处理返回平均耗时',
  userUploadDuration: '用户上传耗时',
  processingDuration: '处理返回耗时',
  userFirstTokenWithUpload: '首字(包含上传耗时)',
  userUploadDurationHint: '服务端开始读取请求体到接收完成的时间，包含等待首个字节；不包含反向代理中已缓冲的时间。',
  processingDurationHint: '请求体接收完成到服务端处理和返回结束的时间，包含排队、重试和输出；不是客户端接收完成时间。',
  requestPhasesHint: '用户上传耗时 / 处理返回耗时。未采集的记录显示 —。',
  requestFirstTokenHint: '优先展示含转发前等待的服务端首字；历史记录沿用原采集口径。',
  requestTimingAveragesHint: '过去 24 小时的均值，按页面范围统计全站、当前用户或当前 API Key；每小时更新，不随日期等筛选变化。基于 {count} 次完整实测请求。耗时明细保留 30 天；未采集、已过期或对应多次实际请求的记录不计入。',
}
