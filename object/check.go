// Copyright 2021 The Casdoor Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package object

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/casdoor/casdoor/cred"
	"github.com/casdoor/casdoor/i18n"
	"github.com/casdoor/casdoor/util"
	goldap "github.com/go-ldap/ldap/v3"
)

var (
	reWhiteSpace     *regexp.Regexp
	reFieldWhiteList *regexp.Regexp
)

const (
	SigninWrongTimesLimit     = 5
	LastSignWrongTimeDuration = time.Minute * 15
)

func init() {
	reWhiteSpace, _ = regexp.Compile(`\s`)
	reFieldWhiteList, _ = regexp.Compile(`^[A-Za-z0-9]+$`)
}

func CheckUserSignup(application *Application, organization *Organization, username string, password string, displayName string, firstName string, lastName string, email string, phone string, affiliation string, lang string) string {
	if organization == nil {
		return i18n.Translate(lang, "OrgErr.DoNotExist")
	}

	if application.IsSignupItemVisible("Username") {
		if len(username) <= 1 {
			return i18n.Translate(lang, "UserErr.NameLessThanTwoCharacters")
		}
		if unicode.IsDigit(rune(username[0])) {
			return i18n.Translate(lang, "UserErr.NameStartWithADigitErr")
		}
		if util.IsEmailValid(username) {
			return i18n.Translate(lang, "UserErr.NameIsEmailErr")
		}
		if reWhiteSpace.MatchString(username) {
			return i18n.Translate(lang, "UserErr.NameCantainWhitSpaceErr")
		}
		msg := CheckUsername(username, lang)
		if msg != "" {
			return msg
		}

		if HasUserByField(organization.Name, "name", username) {
			return i18n.Translate(lang, "UserErr.NameExistedErr")
		}
		if HasUserByField(organization.Name, "email", email) {
			return i18n.Translate(lang, "EmailErr.ExistedErr")
		}
		if HasUserByField(organization.Name, "phone", phone) {
			return i18n.Translate(lang, "PhoneErr.ExistedErr")
		}
	}

	if len(password) <= 5 {
		return i18n.Translate(lang, "UserErr.PasswordLessThanSixCharacters")
	}

	if application.IsSignupItemVisible("Email") {
		if email == "" {
			if application.IsSignupItemRequired("Email") {
				return i18n.Translate(lang, "EmailErr.EmptyErr")
			} else {
				return ""
			}
		}

		if HasUserByField(organization.Name, "email", email) {
			return i18n.Translate(lang, "EmailErr.ExistedErr")
		} else if !util.IsEmailValid(email) {
			return i18n.Translate(lang, "EmailErr.EmailInvalid")
		}
	}

	if application.IsSignupItemVisible("Phone") {
		if phone == "" {
			if application.IsSignupItemRequired("Phone") {
				return i18n.Translate(lang, "PhoneErr.EmptyErr")
			} else {
				return ""
			}
		}

		if HasUserByField(organization.Name, "phone", phone) {
			return i18n.Translate(lang, "PhoneErr.ExistedErr")
		} else if organization.PhonePrefix == "86" && !util.IsPhoneCnValid(phone) {
			return i18n.Translate(lang, "PhoneErr.NumberInvalid")
		}
	}

	if application.IsSignupItemVisible("Display name") {
		if application.GetSignupItemRule("Display name") == "First, last" && (firstName != "" || lastName != "") {
			if firstName == "" {
				return i18n.Translate(lang, "UserErr.FirstNameBlankErr")
			} else if lastName == "" {
				return i18n.Translate(lang, "UserErr.LastNameBlankErr")
			}
		} else {
			if displayName == "" {
				return i18n.Translate(lang, "UserErr.DisplayNameBlankErr")
			} else if application.GetSignupItemRule("Display name") == "Real name" {
				if !isValidRealName(displayName) {
					return i18n.Translate(lang, "UserErr.DisplayNameInvalid")
				}
			}
		}
	}

	if application.IsSignupItemVisible("Affiliation") {
		if affiliation == "" {
			return i18n.Translate(lang, "UserErr.AffiliationBlankErr")
		}
	}

	return ""
}

func checkSigninErrorTimes(user *User, lang string) string {
	if user.SigninWrongTimes >= SigninWrongTimesLimit {
		lastSignWrongTime, _ := time.Parse(time.RFC3339, user.LastSigninWrongTime)
		passedTime := time.Now().UTC().Sub(lastSignWrongTime)
		seconds := int(LastSignWrongTimeDuration.Seconds() - passedTime.Seconds())

		// deny the login if the error times is greater than the limit and the last login time is less than the duration
		if seconds > 0 {
			return fmt.Sprintf(i18n.Translate(lang, "AuthErr.WrongPasswordManyTimes"), seconds/60, seconds%60)
		}

		// reset the error times
		user.SigninWrongTimes = 0

		UpdateUser(user.GetId(), user, []string{"signin_wrong_times"}, user.IsGlobalAdmin)
	}

	return ""
}

func CheckPassword(user *User, password string, lang string) string {
	// check the login error times
	if msg := checkSigninErrorTimes(user, lang); msg != "" {
		return msg
	}

	organization := GetOrganizationByUser(user)
	if organization == nil {
		return i18n.Translate(lang, "OrgErr.DoNotExist")
	}

	credManager := cred.GetCredManager(organization.PasswordType)
	if credManager != nil {
		if organization.MasterPassword != "" {
			if credManager.IsPasswordCorrect(password, organization.MasterPassword, "", organization.PasswordSalt) {
				resetUserSigninErrorTimes(user)
				return ""
			}
		}

		if credManager.IsPasswordCorrect(password, user.Password, user.PasswordSalt, organization.PasswordSalt) {
			resetUserSigninErrorTimes(user)
			return ""
		}

		return recordSigninErrorInfo(user)
	} else {
		return fmt.Sprintf(i18n.Translate(lang, "LoginErr.UnsupportedPasswordType"), organization.PasswordType)
	}
}

func checkLdapUserPassword(user *User, password string, lang string) (*User, string) {
	ldaps := GetLdaps(user.Owner)
	ldapLoginSuccess := false
	for _, ldapServer := range ldaps {
		conn, err := GetLdapConn(ldapServer.Host, ldapServer.Port, ldapServer.Admin, ldapServer.Passwd)
		if err != nil {
			continue
		}
		SearchFilter := fmt.Sprintf("(&(objectClass=posixAccount)(uid=%s))", user.Name)
		searchReq := goldap.NewSearchRequest(ldapServer.BaseDn,
			goldap.ScopeWholeSubtree, goldap.NeverDerefAliases, 0, 0, false,
			SearchFilter, []string{}, nil)
		searchResult, err := conn.Conn.Search(searchReq)
		if err != nil {
			return nil, err.Error()
		}

		if len(searchResult.Entries) == 0 {
			continue
		} else if len(searchResult.Entries) > 1 {
			return nil, i18n.Translate(lang, "LdapErr.MultipleAccounts")
		}

		dn := searchResult.Entries[0].DN
		if err := conn.Conn.Bind(dn, password); err == nil {
			ldapLoginSuccess = true
			break
		}
	}

	if !ldapLoginSuccess {
		return nil, i18n.Translate(lang, "LdapErr.PasswordWrong")
	}
	return user, ""
}

func CheckUserPassword(organization string, username string, password string, lang string) (*User, string) {
	user := GetUserByFields(organization, username)
	if user == nil || user.IsDeleted == true {
		return nil, i18n.Translate(lang, "UserErr.DoNotExistSignUp")
	}

	if user.IsForbidden {
		return nil, i18n.Translate(lang, "LoginErr.UserIsForbidden")
	}

	if user.Ldap != "" {
		// ONLY for ldap users
		return checkLdapUserPassword(user, password, lang)
	} else {
		msg := CheckPassword(user, password, lang)
		if msg != "" {
			return nil, msg
		}
	}
	return user, ""
}

func filterField(field string) bool {
	return reFieldWhiteList.MatchString(field)
}

func CheckUserPermission(requestUserId, userId, userOwner string, strict bool, lang string) (bool, error) {
	if requestUserId == "" {
		return false, fmt.Errorf(i18n.Translate(lang, "LoginErr.LoginFirst"))
	}

	if userId != "" {
		targetUser := GetUser(userId)
		if targetUser == nil {
			return false, fmt.Errorf(i18n.Translate(lang, "UserErr.DoNotExist"), userId)
		}

		userOwner = targetUser.Owner
	}

	hasPermission := false
	if strings.HasPrefix(requestUserId, "app/") {
		hasPermission = true
	} else {
		requestUser := GetUser(requestUserId)
		if requestUser == nil {
			return false, fmt.Errorf(i18n.Translate(lang, "LoginErr.SessionOutdated"))
		}
		if requestUser.IsGlobalAdmin {
			hasPermission = true
		} else if requestUserId == userId {
			hasPermission = true
		} else if userOwner == requestUser.Owner {
			if strict {
				hasPermission = requestUser.IsAdmin
			} else {
				hasPermission = true
			}
		}
	}

	return hasPermission, fmt.Errorf(i18n.Translate(lang, "LoginErr.NoPermission"))
}

func CheckAccessPermission(userId string, application *Application) (bool, error) {
	permissions := GetPermissions(application.Organization)
	allowed := true
	var err error
	for _, permission := range permissions {
		if !permission.IsEnabled || len(permission.Users) == 0 {
			continue
		}

		isHit := false
		for _, resource := range permission.Resources {
			if application.Name == resource {
				isHit = true
				break
			}
		}

		if isHit {
			containsAsterisk := ContainsAsterisk(userId, permission.Users)
			if containsAsterisk {
				return true, err
			}
			enforcer := getEnforcer(permission)
			allowed, err = enforcer.Enforce(userId, application.Name, "read")
			break
		}
	}
	return allowed, err
}

func CheckUsername(username string, lang string) string {
	if username == "" {
		return i18n.Translate(lang, "UserErr.NameEmptyErr")
	} else if len(username) > 39 {
		return i18n.Translate(lang, "UserErr.NameTooLang")
	}

	exclude, _ := regexp.Compile("^[\u0021-\u007E]+$")
	if !exclude.MatchString(username) {
		return ""
	}

	// https://stackoverflow.com/questions/58726546/github-username-convention-using-regex
	re, _ := regexp.Compile("^[a-zA-Z0-9]+((?:-[a-zA-Z0-9]+)|(?:_[a-zA-Z0-9]+))*$")
	if !re.MatchString(username) {
		return i18n.Translate(lang, "UserErr.NameFormatErr")
	}

	return ""
}

func CheckToEnableCaptcha(application *Application) bool {
	if len(application.Providers) == 0 {
		return false
	}

	for _, providerItem := range application.Providers {
		if providerItem.Provider == nil {
			continue
		}
		if providerItem.Provider.Category == "Captcha" && providerItem.Provider.Type == "Default" {
			return providerItem.Rule == "Always"
		}
	}

	return false
}
