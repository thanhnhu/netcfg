// Reveals a password field so a typed passphrase can be checked before it is sent.
(function () {
    for (const btn of document.querySelectorAll("button.pw-toggle")) {
        const input = document.getElementById(btn.dataset.target);
        if (!input) continue;

        btn.hidden = false;
        btn.addEventListener("click", function () {
            const reveal = input.type === "password";
            input.type = reveal ? "text" : "password";
            btn.textContent = reveal ? btn.dataset.hide : btn.dataset.show;
            btn.setAttribute("aria-pressed", String(reveal));
            input.focus();
        });
    }
})();
