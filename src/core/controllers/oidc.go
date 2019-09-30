// Copyright 2018 Project Harbor Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/goharbor/harbor/src/common"
	"github.com/goharbor/harbor/src/common/dao"
	"github.com/goharbor/harbor/src/common/models"
	"github.com/goharbor/harbor/src/common/utils"
	"github.com/goharbor/harbor/src/common/utils/log"
	"github.com/goharbor/harbor/src/common/utils/oidc"
	"github.com/goharbor/harbor/src/core/api"
	"github.com/goharbor/harbor/src/core/config"
	"github.com/pkg/errors"
)

const tokenKey = "oidc_token"
const stateKey = "oidc_state"
const userInfoKey = "oidc_user_info"
const oidcUserComment = "Onboarded via OIDC provider"

// OIDCController handles requests for OIDC login, callback and user onboard
type OIDCController struct {
	api.BaseController
}

type onboardReq struct {
	Username string `json:"username"`
}

type oidcUserData struct {
	Issuer   string `json:"iss"`
	Subject  string `json:"sub"`
	Username string `json:"name"`
	Email    string `json:"email"`
}

// Prepare include public code path for call request handler of OIDCController
func (oc *OIDCController) Prepare() {
	if mode, _ := config.AuthMode(); mode != common.OIDCAuth {
		oc.SendPreconditionFailedError(fmt.Errorf("Auth Mode: %s is not OIDC based", mode))
		return
	}
}

// RedirectLogin redirect user's browser to OIDC provider's login page
func (oc *OIDCController) RedirectLogin() {
	state := utils.GenerateRandomString()
	url, err := oidc.AuthCodeURL(state)
	if err != nil {
		oc.SendInternalServerError(err)
		return
	}
	oc.SetSession(stateKey, state)
	log.Debugf("State dumped to session: %s", state)
	// Force to use the func 'Redirect' of beego.Controller
	oc.Controller.Redirect(url, http.StatusFound)
}

// Callback handles redirection from OIDC provider.  It will exchange the token and
// kick off onboard if needed.
func (oc *OIDCController) Callback() {
	if oc.Ctx.Request.URL.Query().Get("state") != oc.GetSession(stateKey) {
		log.Errorf("State mismatch, in session: %s, in url: %s", oc.GetSession(stateKey),
			oc.Ctx.Request.URL.Query().Get("state"))
		oc.SendBadRequestError(errors.New("State mismatch"))
		return
	}

	errorCode := oc.Ctx.Request.URL.Query().Get("error")
	if errorCode != "" {
		errorDescription := oc.Ctx.Request.URL.Query().Get("error_description")
		log.Errorf("OIDC callback returned error: %s - %s", errorCode, errorDescription)
		oc.SendBadRequestError(errors.Errorf("OIDC callback returned error: %s - %s", errorCode, errorDescription))
		return
	}

	code := oc.Ctx.Request.URL.Query().Get("code")
	ctx := oc.Ctx.Request.Context()
	token, err := oidc.ExchangeToken(ctx, code)
	if err != nil {
		log.Errorf("Failed to exchange token, error: %v", err)
		// Return a 4xx error so user can see the details in case it's due to misconfiguration.
		oc.SendBadRequestError(err)
		return
	}
	log.Debugf("ID token from provider: %s", token.IDToken)

	idToken, err := oidc.VerifyToken(ctx, token.IDToken)
	if err != nil {
		oc.SendInternalServerError(err)
		return
	}
	d := &oidcUserData{}
	err = idToken.Claims(d)
	if err != nil {
		oc.SendInternalServerError(err)
		return
	}
	ouDataStr, err := json.Marshal(d)
	if err != nil {
		oc.SendInternalServerError(err)
		return
	}
	u, err := dao.GetUserBySubIss(d.Subject, d.Issuer)
	if err != nil {
		oc.SendInternalServerError(err)
		return
	}

	tokenBytes, err := json.Marshal(token)
	if err != nil {
		oc.SendInternalServerError(err)
		return
	}
	oc.SetSession(tokenKey, tokenBytes)

	oidcSettings, err := config.OIDCSetting()
	if err != nil {
		oc.SendInternalServerError(err)
		return
	}

	if u == nil {
		// Recover the username from d.Username by default
		username := d.Username

		// Check if there is a custom username claim setting
		if oidcSettings.UserClaim != "" {
			// Generic unmarshall to retrieve the custom claim
			allClaims := make(map[string]interface{})
			err = idToken.Claims(&allClaims)
			if err != nil {
				oc.SendInternalServerError(err)
				return
			}

			username = allClaims[oidcSettings.UserClaim].(string)
		}

		// Fix blanks in username
		username = strings.Replace(username, " ", "_", -1)

		// If automatic onboard is enabled, skip the onboard page
		if oidcSettings.AutoOnboard {
			log.Error("**AIM** automatic onboarding\n")
			user, onboarded := userOnboard(oc, d, username, tokenBytes)
			if onboarded == false {
				log.Error("**AIM** Not onboarded\n")
				return
			}
			log.Error("**AIM** Onboarded\n")
			u = user

		} else {
			log.Error("**AIM** User is null\n")
			oc.SetSession(userInfoKey, string(ouDataStr))
			oc.Controller.Redirect(fmt.Sprintf("/oidc-onboard?username=%s", username), http.StatusFound)
			// Once redirected, no further actions are done
			return
		}

	}

	log.Errorf("**AIM** User is %+v\n", u)

	//TODO: Update user details (email, etc.) from OIDC token?
	oidcUser, err := dao.GetOIDCUserByUserID(u.UserID)
	if err != nil {
		oc.SendInternalServerError(err)
		return
	}
	_, t, err := secretAndToken(tokenBytes)
	oidcUser.Token = t
	if err := dao.UpdateOIDCUser(oidcUser); err != nil {
		oc.SendInternalServerError(err)
		return
	}
	oc.PopulateUserSession(*u)
	oc.Controller.Redirect("/", http.StatusFound)

}

func userOnboard(oc *OIDCController, d *oidcUserData, username string, tokenBytes []byte) (*models.User, bool) {

	s, t, err := secretAndToken(tokenBytes)
	if err != nil {
		log.Error("**AIM** Error with secret and token\n")
		oc.SendInternalServerError(err)
		return nil, false
	}

	oidcUser := models.OIDCUser{
		SubIss: d.Subject + d.Issuer,
		Secret: s,
		Token:  t,
	}

	log.Errorf("**AIM** OIDC user created: %v\n", oidcUser)

	email := d.Email
	user := models.User{
		Username:     username,
		Realname:     username,
		Email:        email,
		OIDCUserMeta: &oidcUser,
		Comment:      oidcUserComment,
	}

	log.Errorf("**AIM** User created: %+v\n", user)

	err = dao.OnBoardOIDCUser(&user)
	if err != nil {
		if strings.Contains(err.Error(), dao.ErrDupUser.Error()) {
			oc.RenderError(http.StatusConflict, "Conflict in username, the user with same username has been onboarded.")
			return nil, false
		}

		oc.SendInternalServerError(err)
		oc.DelSession(userInfoKey)
		return nil, false
	}

	return &user, true
}

// Onboard handles the request to onboard a user authenticated via OIDC provider
func (oc *OIDCController) Onboard() {
	u := &onboardReq{}
	if err := oc.DecodeJSONReq(u); err != nil {
		oc.SendBadRequestError(err)
		return
	}
	username := u.Username
	if utils.IsIllegalLength(username, 1, 255) {
		oc.SendBadRequestError(errors.New("username with illegal length"))
		return
	}
	if utils.IsContainIllegalChar(username, []string{",", "~", "#", "$", "%"}) {
		oc.SendBadRequestError(errors.New("username contains illegal characters"))
		return
	}

	userInfoStr, ok := oc.GetSession(userInfoKey).(string)
	if !ok {
		oc.SendBadRequestError(errors.New("Failed to get OIDC user info from session"))
		return
	}
	log.Debugf("User info string: %s\n", userInfoStr)
	tb, ok := oc.GetSession(tokenKey).([]byte)
	if !ok {
		oc.SendBadRequestError(errors.New("Failed to get OIDC token from session"))
		return
	}

	d := &oidcUserData{}
	err := json.Unmarshal([]byte(userInfoStr), &d)
	if err != nil {
		oc.SendInternalServerError(err)
		return
	}

	user, onboarded := userOnboard(oc, d, username, tb)

	if onboarded {
		user.OIDCUserMeta = nil
		oc.DelSession(userInfoKey)
		oc.PopulateUserSession(*user)
	}
}

func secretAndToken(tokenBytes []byte) (string, string, error) {
	key, err := config.SecretKey()
	if err != nil {
		return "", "", err
	}
	token, err := utils.ReversibleEncrypt((string)(tokenBytes), key)
	if err != nil {
		return "", "", err
	}
	str := utils.GenerateRandomString()
	secret, err := utils.ReversibleEncrypt(str, key)
	if err != nil {
		return "", "", err
	}
	return secret, token, nil
}
