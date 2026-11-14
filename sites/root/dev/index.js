const toggle = document.getElementById('dev-toggle');

if (toggle) {
    fetch('/_dev/status', { cache: 'no-store' })
        .then((response) => response.json())
        .then(({ enabled }) => {
            if (!enabled) {
                return;
            }

            toggle.textContent = 'Leave dev';
            toggle.href = '/_dev?off=1&next=/dev/';
        })
        .catch(() => {});
}
