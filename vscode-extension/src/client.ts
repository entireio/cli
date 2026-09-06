import * as http from 'http';

export interface ReadinessStatus {
    entire_installed: boolean;
    entire_enabled: boolean;
    graph_available: boolean;
    checkpoints_count: number;
    readiness_score: number;
    agent_integration: string;
    redaction_active: boolean;
    status: string;
}

export interface CheckpointItem {
    checkpoint_id: string;
    commit_ref: string;
    timestamp: string;
    intent_context: string;
    files_changed: string[];
    verification_info: string;
}

export interface RequirementItem {
    id: string;
    title: string;
    description: string;
    status: string;
    verification_evidence: string;
}

export interface GraphFindingItem {
    id: string;
    query_change: string;
    affected_files: string[];
    risk_information: string;
    verification_status: string;
}

export interface HandoffItem {
    id: string;
    original_intent: string;
    completed_work: string[];
    remaining_work: string[];
    risks: string[];
    recommended_next_action: string;
}

export class CheckpointApiClient {
    private baseUrl: string = 'http://localhost:8080';

    public async getReadiness(): Promise<ReadinessStatus> {
        return this.fetchJson<ReadinessStatus>('/api/readiness');
    }

    public async enableEntire(): Promise<{ status: string; message: string }> {
        return this.fetchJson<{ status: string; message: string }>('/api/enable', 'POST');
    }

    public async getCheckpoints(repoId: string = 'repo-cli-btw'): Promise<CheckpointItem[]> {
        return this.fetchJson<CheckpointItem[]>(`/api/repositories/${repoId}/checkpoints`);
    }

    public async getRequirements(repoId: string = 'repo-cli-btw'): Promise<RequirementItem[]> {
        return this.fetchJson<RequirementItem[]>(`/api/repositories/${repoId}/requirements`);
    }

    public async getGraphFindings(repoId: string = 'repo-cli-btw'): Promise<GraphFindingItem[]> {
        return this.fetchJson<GraphFindingItem[]>(`/api/repositories/${repoId}/graph`);
    }

    public async getHandoff(repoId: string = 'repo-cli-btw'): Promise<HandoffItem> {
        return this.fetchJson<HandoffItem>(`/api/repositories/${repoId}/handoff`);
    }

    private fetchJson<T>(path: string, method: string = 'GET'): Promise<T> {
        return new Promise((resolve, reject) => {
            const req = http.request(`${this.baseUrl}${path}`, { method }, (res) => {
                let data = '';
                res.on('data', chunk => data += chunk);
                res.on('end', () => {
                    try {
                        resolve(JSON.parse(data));
                    } catch (e) {
                        reject(e);
                    }
                });
            });
            req.on('error', (err) => reject(err));
            req.end();
        });
    }
}
