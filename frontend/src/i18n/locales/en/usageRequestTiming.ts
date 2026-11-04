export default {
  avgUserUpload: 'Average user upload time',
  avgProcessing: 'Average processing and return time',
  userUploadDuration: 'User upload time',
  processingDuration: 'Processing and return time',
  userUploadDurationHint: 'From the server’s first request-body read to receiving the complete body, including the first-byte wait. Time already buffered by a reverse proxy is not observed.',
  processingDurationHint: 'From receiving the complete request body to finishing processing and local response writes, including queues, retries and output. This is not client receipt time.',
  requestPhasesHint: 'User upload time / processing and return time. Uncollected measurements are shown as —.',
  requestFirstTokenHint: 'Uses server first-token time including pre-forward waits when measured; older records retain their original timing scope.',
  requestTimingAveragesHint: 'Averages over {count} requests with complete measurements in the selected scope. Timing details are retained for 30 days; uncollected, expired or ambiguous records are excluded.',
}
