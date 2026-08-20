// Closes the header account menu on an outside click or Escape, which a bare
// <details> element does not do on its own.
(function () {
    const menu = document.querySelector("details.usermenu");
    if (!menu) return;

    document.addEventListener("click", function (event) {
        if (!menu.contains(event.target)) menu.open = false;
    });
    document.addEventListener("keydown", function (event) {
        if (event.key === "Escape") menu.open = false;
    });
})();
