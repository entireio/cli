document.addEventListener('DOMContentLoaded', () => {
    const API_BASE = '/api';

    // Navigation Tab Switching
    const navItems = document.querySelectorAll('.nav-item');
    const tabPanes = document.querySelectorAll('.tab-pane');
    const pageTitle = document.getElementById('page-title');

    const tabTitles = {
        overview: 'Release Readiness Overview',
        checkpoints: 'Entire Checkpoints Log',
        requirements: 'Requirements & Milestones Matrix',
        graph: 'Entire Graph Structural Impact Analysis',
        handoff: 'Agent & Developer Handoff Package'
    };

    navItems.forEach(item => {
        item.addEventListener('click', () => {
            const targetTab = item.getAttribute('data-tab');

            navItems.forEach(i => i.classList.remove('active'));
            tabPanes.forEach(p => p.classList.remove('active'));

            item.classList.add('active');
            const activePane = document.getElementById(`tab-${targetTab}`);
            if (activePane) activePane.classList.add('active');

            if (pageTitle && tabTitles[targetTab]) {
                pageTitle.textContent = tabTitles[targetTab];
            }
        });
    });

    // Fetch API Data
    fetchHealthStatus();
    fetchRepoData();
    fetchCheckpoints();
    fetchRequirements();
    fetchGraphData();
    fetchHandoffData();

    async function fetchHealthStatus() {
        try {
            const res = await fetch(`${API_BASE}/health`);
            const data = await res.json();
            const el = document.getElementById('health-status');
            if (el && data.status === 'ok') {
                el.textContent = `Online • ${data.service}`;
                el.style.color = '#10b981';
            }
        } catch (err) {
            const el = document.getElementById('health-status');
            if (el) el.textContent = 'API Unavailable';
        }
    }

    async function fetchRepoData() {
        try {
            const res = await fetch(`${API_BASE}/repositories/repo-cli-btw`);
            const repo = await res.json();
            const container = document.getElementById('repo-details');
            if (container) {
                container.innerHTML = `
                    <div><strong>Repository:</strong> ${repo.owner}/${repo.name}</div>
                    <div><strong>Default Branch:</strong> ${repo.default_branch}</div>
                    <div><strong>Local Path:</strong> <code>${repo.local_path}</code></div>
                    <div><strong>Description:</strong> ${repo.description}</div>
                `;
            }
        } catch (err) {
            console.error('Failed to fetch repo:', err);
        }
    }

    async function fetchCheckpoints() {
        try {
            const res = await fetch(`${API_BASE}/repositories/repo-cli-btw/checkpoints`);
            const cps = await res.json();
            const tbody = document.getElementById('checkpoints-table-body');
            const countEl = document.getElementById('checkpoint-count');
            if (countEl) countEl.textContent = cps.length;

            if (tbody) {
                tbody.innerHTML = cps.map(cp => `
                    <tr>
                        <td><code>${cp.checkpoint_id}</code></td>
                        <td><code>${cp.commit_ref}</code></td>
                        <td>${new Date(cp.timestamp).toLocaleString()}</td>
                        <td>${cp.intent_context}</td>
                        <td><span class="status-badge completed">VERIFIED</span></td>
                    </tr>
                `).join('');
            }
        } catch (err) {
            console.error('Failed to fetch checkpoints:', err);
        }
    }

    async function fetchRequirements() {
        try {
            const res = await fetch(`${API_BASE}/repositories/repo-cli-btw/requirements`);
            const reqs = await res.json();
            const tbody = document.getElementById('requirements-table-body');
            if (tbody) {
                tbody.innerHTML = reqs.map(r => `
                    <tr>
                        <td><code>${r.id}</code></td>
                        <td><strong>${r.title}</strong><br><small>${r.description}</small></td>
                        <td><span class="status-badge ${r.status}">${r.status.toUpperCase()}</span></td>
                        <td>${r.related_checkpoints.map(c => `<code>${c}</code>`).join(', ')}</td>
                        <td>${r.verification_evidence}</td>
                    </tr>
                `).join('');
            }
        } catch (err) {
            console.error('Failed to fetch requirements:', err);
        }
    }

    async function fetchGraphData() {
        try {
            const res = await fetch(`${API_BASE}/repositories/repo-cli-btw/graph`);
            const findings = await res.json();
            const container = document.getElementById('graph-findings-list');
            if (container) {
                container.innerHTML = findings.map(f => `
                    <div style="margin-bottom: 16px;">
                        <h4>${f.id}: ${f.query_change}</h4>
                        <p style="margin-top: 4px; color: var(--text-secondary);">Affected Files: ${f.affected_files.join(', ')}</p>
                        <p style="color: var(--text-secondary);">Functions: ${f.affected_functions.join(', ')}</p>
                        <p style="color: var(--accent-cyan);">Evidence: ${f.source_evidence}</p>
                    </div>
                `).join('');
            }
        } catch (err) {
            console.error('Failed to fetch graph data:', err);
        }
    }

    async function fetchHandoffData() {
        try {
            const res = await fetch(`${API_BASE}/repositories/repo-cli-btw/handoff`);
            const h = await res.json();
            const container = document.getElementById('handoff-content');
            if (container) {
                container.innerHTML = `
                    <p><strong>Original Intent:</strong> ${h.original_intent}</p>
                    <br>
                    <h4>Completed Work:</h4>
                    <ul>${h.completed_work.map(w => `<li>${w}</li>`).join('')}</ul>
                    <br>
                    <h4>Remaining Tasks:</h4>
                    <ul>${h.remaining_work.map(r => `<li>${r}</li>`).join('')}</ul>
                    <br>
                    <p><strong>Recommended Action:</strong> ${h.recommended_next_action}</p>
                `;
            }
        } catch (err) {
            console.error('Failed to fetch handoff:', err);
        }
    }
});
