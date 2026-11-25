"use strict";

const express = require("express");
const url = require("url");
const urljoin = require("url-join");
const csrf = require("csurf");
const { hydraAdmin } = require("../config");
const { oidcConformityMaybeFakeAcr } = require("./stub/oidc-cert");

// Development accounts only. The subject matches the email, which is the
// same pattern as the original single-user login route.
const users = {
  "foo@bar.com": { password: "foobar", subject: "foo@bar.com" },
  "bar@foo.com": { password: "foobar", subject: "bar@foo.com" },
  "baz@foo.com": { password: "foobar", subject: "baz@foo.com" },
};

const csrfProtection = csrf({
  cookie: {
    sameSite: "lax",
  },
});

const router = express.Router();

function loginAction() {
  return urljoin(process.env.BASE_URL || "", "/login");
}

function findUser(email, password) {
  const user = users[email];
  if (!user || user.password !== password) {
    return undefined;
  }
  return user;
}

router.get("/", csrfProtection, (req, res, next) => {
  const query = url.parse(req.url, true).query;
  const challenge = String(query.login_challenge || "");
  if (!challenge) {
    next(new Error("Expected a login challenge to be set but received none."));
    return;
  }

  hydraAdmin
    .adminGetOAuth2LoginRequest(challenge)
    .then(({ data: body }) => {
      if (body.skip) {
        return hydraAdmin
          .adminAcceptOAuth2LoginRequest(challenge, {
            subject: String(body.subject),
          })
          .then(({ data: accept }) => {
            res.redirect(String(accept.redirect_to));
          });
      }

      const hint =
        body.oidc_context && body.oidc_context.login_hint
          ? body.oidc_context.login_hint
          : "";
      res.render("login", {
        csrfToken: req.csrfToken(),
        challenge: challenge,
        action: loginAction(),
        hint: hint,
      });
      return undefined;
    })
    .catch(next);
});

router.post("/", csrfProtection, (req, res, next) => {
  const challenge = req.body.challenge;

  if (req.body.submit === "Deny access") {
    hydraAdmin
      .adminRejectOAuth2LoginRequest(challenge, {
        error: "access_denied",
        error_description: "The resource owner denied the request",
      })
      .then(({ data: body }) => {
        res.redirect(String(body.redirect_to));
      })
      .catch(next);
    return;
  }

  const user = findUser(req.body.email, req.body.password);
  if (!user) {
    res.render("login", {
      csrfToken: req.csrfToken(),
      challenge: challenge,
      action: loginAction(),
      error: "The username / password combination is not correct",
    });
    return;
  }

  hydraAdmin
    .adminGetOAuth2LoginRequest(challenge)
    .then(({ data: loginRequest }) =>
      hydraAdmin.adminAcceptOAuth2LoginRequest(challenge, {
        subject: user.subject,
        remember: Boolean(req.body.remember),
        remember_for: 3600,
        acr: oidcConformityMaybeFakeAcr(loginRequest, "0"),
      }),
    )
    .then(({ data: body }) => {
      res.redirect(String(body.redirect_to));
    })
    .catch(next);
});

exports.__esModule = true;
exports.default = router;
