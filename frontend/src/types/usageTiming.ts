export interface RequestPhases {
  user_upload_ms: number | null
  processing_ms: number | null
}

export interface RequestPhaseAverages {
  average_user_upload_ms?: number | null
  average_processing_ms?: number | null
  timing_sample_count?: number
}
