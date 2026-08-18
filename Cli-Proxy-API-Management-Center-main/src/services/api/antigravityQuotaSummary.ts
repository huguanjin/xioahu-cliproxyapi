import { apiClient } from './client';

export interface AntigravityQuotaSummaryResponse {
  generated_at: string;
  total_antigravity_credentials: number;
  gemini: {
    available: number;
    total: number;
  };
  claude: {
    available: number;
    total: number;
    available_via_credits_fallback_only: number;
    credits_fallback_enabled: boolean;
  };
}

export const antigravityQuotaSummaryApi = {
  get: () => apiClient.get<AntigravityQuotaSummaryResponse>('/antigravity-quota-summary'),
};
